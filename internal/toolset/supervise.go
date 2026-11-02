package toolset

import (
	"context"
	"net"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/monitor"
)

// Supervisor is the guard the configuration's monitor section asks for, or
// nil when it is off. unattended is set where nobody can answer an ask: a
// -p run, an SDK agent without an approver, an unattended server run.
func Supervisor(cfg config.Config, reg *tools.Registry, unattended bool) *monitor.Guard {
	m := cfg.Monitor
	if !m.Enabled {
		return nil
	}
	look := hostLookPath(reg)
	var home func() (string, bool)
	if look != nil { // commands run on this machine's files: ~ is the HOME they get (the fence's temp home on that tier)
		home = tools.CommandHome(reg)
	}
	var judge monitor.Monitor = monitor.Rules{LookPath: look, Declared: declared(cfg), Hosts: sshHosts(cfg), Home: home}
	if m.Judge == "model" {
		var model monitor.Monitor = monitor.Model{Endpoint: m.Endpoint, Model: m.Model, APIKey: apiKey(m.APIKeyEnv)}
		// A judge model the allowed models refuse is never asked: every call
		// it would judge gets what an unavailable judge gives.
		if err := cfg.JudgeListed(m); err != nil {
			model = refusedJudge{err}
		}
		judge = monitor.Chain{judge, model}
	}
	g := &monitor.Guard{Monitor: judge, MaxPerTurn: m.MaxPerTurn, Unattended: unattended, OnUnavailable: policy.Ask,
		Honeytokens: append([]string(nil), m.Honeytokens...)}
	if m.OnUnavailable == "deny" {
		g.OnUnavailable = policy.Deny
	}
	if m.TimeoutMS > 0 {
		g.Timeout = time.Duration(m.TimeoutMS) * time.Millisecond
	}
	return g
}

// refusedJudge is a model judge the configuration does not allow.
type refusedJudge struct{ err error }

func (r refusedJudge) Judge(context.Context, monitor.Case) (monitor.Verdict, error) {
	return monitor.Verdict{}, r.err
}

// declared are the ssh hosts and Kubernetes clusters the operator configured.
func declared(cfg config.Config) []string {
	var out []string
	for _, h := range cfg.SSH.Hosts {
		out = append(out, h.Name)
		if host, _, err := net.SplitHostPort(h.Addr); err == nil {
			out = append(out, host)
		} else {
			out = append(out, h.Addr)
		}
	}
	for _, c := range cfg.K8s.Clusters {
		out = append(out, c.Name)
	}
	return out
}

// sshHosts are the configured ssh hosts by name and address.
func sshHosts(cfg config.Config) []monitor.DeclaredHost {
	var out []monitor.DeclaredHost
	for _, h := range cfg.SSH.Hosts {
		addr := h.Addr
		if host, _, err := net.SplitHostPort(h.Addr); err == nil {
			addr = host
		}
		out = append(out, monitor.DeclaredHost{Name: h.Name, Addr: addr})
	}
	return out
}

func apiKey(env string) string {
	if env == "" {
		return ""
	}
	return os.Getenv(env)
}

// hostLookPath finds programs where commands run on the host's own
// filesystem; where they run in a container or a VM it is nil, and the
// rules leave programs unchecked.
func hostLookPath(reg *tools.Registry) func(string) bool {
	if reg == nil {
		return nil
	}
	t, ok := reg.Get("bash")
	if !ok {
		return nil
	}
	b, ok := t.(tools.Bash)
	if !ok || b.RanUnder != nil {
		return nil
	}
	switch b.Isolation.Tier {
	case "", "none", "process", "fence":
		// The PATH commands get: the host's, with the toolchains' first.
		return func(p string) bool {
			if _, err := exec.LookPath(p); err == nil {
				return true
			}
			return b.Provides != nil && b.Provides("", p)
		}
	}
	return nil
}

// ProgramFinder reports whether a program is on the PATH the session's
// commands get, toolchains included, or is nil where that cannot be told
// from here (commands in a container or a VM).
func ProgramFinder(reg *tools.Registry) func(string) bool { return hostLookPath(reg) }

// SupervisorLabel says what supervises calls, for the banner, /status and doctor.
func SupervisorLabel(cfg config.Config) string {
	m := cfg.Monitor
	switch m.JudgeName() {
	case "off":
		return "off"
	case "model":
		where := m.Endpoint
		if u, err := url.Parse(m.Endpoint); err == nil {
			where = u.Scheme + "://" + u.Host
		}
		return "model (" + m.Model + " at " + where + ", after the rules)"
	}
	return "rules"
}
