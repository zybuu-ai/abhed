package toolset

import (
	"os"
	"os/user"
	"strconv"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// LocalPrincipal is who runs this process: the account, and, when agentWhy
// says the process runs inside an agent's command, that command.
func LocalPrincipal(agentWhy string) agent.Principal {
	p := agent.Principal{Kind: "os-user", UID: strconv.Itoa(os.Getuid())}
	if u, err := user.Current(); err == nil {
		p.OSUser = u.Username
	}
	if agentWhy != "" {
		p.Kind, p.AgentCommand = "agent-command", agentWhy
		p.CommandID = os.Getenv("ABHED_COMMAND_ID")
	}
	return p
}

// ConfigAttempts are cfg's settings that did not take effect as written, each
// credited to p.
func ConfigAttempts(cfg config.Config, p agent.Principal) []agent.ConfigAttempt {
	var out []agent.ConfigAttempt
	for _, a := range cfg.Attempts() {
		out = append(out, agent.ConfigAttempt{Layer: a.Layer, Source: config.Printable(a.Source), Key: a.Key,
			Value: config.Printable(a.Value), Decision: a.Decision, Reason: a.Reason, Principal: p})
	}
	return out
}

// RecordConfigAttempts writes each attempt into rec: config.narrowed for a
// narrowing, which took effect, and config.refused for the rest.
func RecordConfigAttempts(rec *agent.Recorder, attempts []agent.ConfigAttempt) error {
	for _, a := range attempts {
		t := agent.EvConfigRefused
		if a.Decision == "narrowed" {
			t = agent.EvConfigNarrowed
		}
		if _, err := rec.Record(t, agent.ActorSystem, agent.Trusted, a); err != nil {
			return err
		}
	}
	return nil
}

// WebState is what session.started says about the web tools: whether each
// is on and who decided it.
func WebState(cfg config.Config) map[string]any {
	source := func(key string, on bool) string {
		switch {
		case on || cfg.ManagedSets(key):
			return "managed"
		case cfg.Sets(key):
			return "narrowed"
		}
		return "default"
	}
	out := map[string]any{
		"search": cfg.WebSearch.Enabled, "search_source": source("web_search.enabled", cfg.WebSearch.Enabled),
		"fetch": cfg.WebFetch.Enabled, "fetch_source": source("web_fetch.enabled", cfg.WebFetch.Enabled),
		"state": cfg.WebSearchState(),
	}
	if cfg.WebSearch.Enabled {
		out["provider"] = cfg.WebSearch.Provider
	}
	return out
}
