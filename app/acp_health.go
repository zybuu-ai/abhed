package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/store/local"
)

// Health (docs/architecture/studio-acp-contract.md §8.1): the doctor's checks
// as structured results, the same for `abhed doctor --json` and Studio. The
// model endpoint is reached only when it is on this machine, and no MCP
// server is started: a check must not be a way to make traffic.

func init() {
	liveFeatures = append(liveFeatures, "doctor")
	handle(map[string]func(*acpConn, rpcMessage){"_abhed/doctor": (*acpConn).doctorACP})
}

// doctorCheck is one check's result.
type doctorCheck struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"` // ok, warn or fail
	Detail string `json:"detail"`
}

// doctorChecks runs the checks for a workspace. rec, when set, checks the
// local record's index too.
func doctorChecks(ctx context.Context, cfg config.Config, cfgErr error, workspace string, verifyIndex func() error) []doctorCheck {
	var out []doctorCheck
	add := func(id, title, status, detail string) {
		out = append(out, doctorCheck{ID: id, Title: title, Status: status, Detail: redacted(detail)})
	}
	if cfgErr != nil {
		add("config", "Configuration", "fail", cfgErr.Error())
		return out
	}
	switch {
	case len(cfg.Unknown) > 0:
		add("config", "Configuration", "warn", fmt.Sprintf("%d setting(s) no part of Abhed reads", len(cfg.Unknown)))
	default:
		add("config", "Configuration", "ok", "loaded")
	}
	ws := cfg.Workspace
	switch {
	case ws.File == "":
		add("trust", "Workspace trust", "ok", "the workspace has no configuration file")
	case ws.Trusted:
		add("trust", "Workspace trust", "ok", "the workspace's configuration file is trusted ("+ws.Reason+")")
	default:
		add("trust", "Workspace trust", "warn", fmt.Sprintf("the workspace's configuration file is not trusted (%s); %d setting(s) ignored", ws.Reason, len(ws.Ignored)))
	}
	provider, err := cfg.Provider()
	if err != nil {
		add("provider", "Model provider", "fail", err.Error())
	} else {
		status, detail := providerReach(ctx, provider.BaseURL)
		add("provider", "Model provider", status, detail)
	}
	if sb, err := buildSandbox(cfg, workspace); err != nil {
		add("sandbox", "Sandbox", "fail", err.Error())
	} else {
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		got, err := sb.Command(sctx, workspace, "echo abhed-sandbox-ok").CombinedOutput()
		cancel()
		switch {
		case err != nil || !strings.Contains(string(got), "abhed-sandbox-ok"):
			add("sandbox", "Sandbox", "fail", fmt.Sprintf("the %s tier could not run a command", sb.Tier()))
		case sb.Tier() == sandbox.TierNone:
			add("sandbox", "Sandbox", "warn", "commands run on the host, with no sandbox")
		default:
			add("sandbox", "Sandbox", "ok", "commands run under the "+string(sb.Tier())+" tier")
		}
	}
	switch {
	case cfg.Storage.Driver == "postgres":
		add("record", "Record", "ok", "kept in Postgres by the server; abhed doctor checks the connection")
	case verifyIndex == nil:
		add("record", "Record", "warn", "this engine keeps no durable record")
	default:
		if err := verifyIndex(); err != nil {
			add("record", "Record", "fail", err.Error())
		} else {
			add("record", "Record", "ok", "the local record's index verifies")
		}
	}
	if n := len(cfg.MCP.Servers); n > 0 {
		add("mcp", "MCP servers", "ok", fmt.Sprintf("%d configured; started with a session", n))
	} else {
		add("mcp", "MCP servers", "ok", "none configured")
	}
	if cfg.Retrieval.Enabled {
		add("index", "Code index", "ok", "enabled; built with a session")
	} else {
		add("index", "Code index", "ok", "off")
	}
	if cfg.Managed {
		add("managed", "Managed policy", "ok", fmt.Sprintf("in force from %s; it sets %d setting(s)", managed.ConfigFile, len(cfg.ManagedKeys)))
	} else {
		add("managed", "Managed policy", "ok", "none")
	}
	return out
}

// providerReach checks a model endpoint on this machine by connecting to it;
// one elsewhere is not contacted.
func providerReach(ctx context.Context, base string) (status, detail string) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "warn", "the endpoint is not a URL this check can read"
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "ok", "configured; not contacted, since it is not on this machine"
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443"}[u.Scheme]
		if port == "" {
			port = "80"
		}
	}
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return "fail", "nothing answers at " + net.JoinHostPort(host, port)
	}
	_ = conn.Close()
	return "ok", "reachable at " + net.JoinHostPort(host, port)
}

func (c *acpConn) doctorACP(msg rpcMessage) {
	var p struct {
		Cwd string `json:"cwd"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	if p.Cwd == "" {
		p.Cwd = c.base
	}
	cfg, err := config.LoadWith(p.Cwd, config.LoadOptions{Trust: config.GrantFor(c.trust, c.base, p.Cwd)})
	var verify func() error
	if c.durable() {
		verify = func() error {
			rec, err := c.record()
			if err != nil {
				return err
			}
			return indexVerified(rec.VerifyIndex())
		}
	}
	c.reply(msg.ID, map[string]any{"checks": doctorChecks(c.root(), cfg, err, p.Cwd, verify)}, nil)
}

// indexVerified turns an index report into the error a check shows.
func indexVerified(rep local.Report, err error) error {
	if err != nil {
		return err
	}
	if !rep.OK {
		return fmt.Errorf("the record's index fails verification at line %d: %s", rep.Line, rep.Reason)
	}
	return nil
}

// doctorJSON is `abhed doctor --json`: the same checks, one JSON object.
func (a *App) doctorJSON(w io.Writer, workspace string) int {
	cfg, err := a.loadConfig(workspace)
	c := &acpConn{}
	c.useRecord(workspace, a.trust)
	var verify func() error
	if c.durable() {
		verify = func() error {
			rec, err := c.record()
			if err != nil {
				return err
			}
			return indexVerified(rec.VerifyIndex())
		}
	}
	checks := doctorChecks(context.Background(), cfg, err, workspace, verify)
	c.closeRecord()
	b, _ := json.MarshalIndent(map[string]any{"checks": checks}, "", "  ")
	fmt.Fprintln(w, string(b))
	for _, ch := range checks {
		if ch.Status == "fail" {
			return 1
		}
	}
	return 0
}
