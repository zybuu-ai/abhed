package toolset

import (
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
	var judge monitor.Monitor = monitor.Rules{LookPath: hostLookPath(reg), Declared: declared(cfg), Hosts: sshHosts(cfg)}
	if m.Judge == "model" {
		judge = monitor.Chain{judge, monitor.Model{Endpoint: m.Endpoint, Model: m.Model, APIKey: apiKey(m.APIKeyEnv)}}
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
		return func(p string) bool { _, err := exec.LookPath(p); return err == nil }
	}
	return nil
}

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
