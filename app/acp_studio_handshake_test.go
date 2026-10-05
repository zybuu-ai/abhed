package app

import (
	"slices"
	"strings"
	"testing"
)

// §1.4: the handshake names the contract level, the edition, the record and
// every area served, and never advertises deletion.
func TestStudioHandshake(t *testing.T) {
	r := newStudioRig(t, "")
	var res struct {
		AgentCapabilities struct {
			LoadSession         bool           `json:"loadSession"`
			SessionCapabilities map[string]any `json:"sessionCapabilities"`
			Meta                map[string]struct {
				APILevel int      `json:"apiLevel"`
				Edition  string   `json:"edition"`
				Version  string   `json:"version"`
				Commit   string   `json:"commit"`
				Features []string `json:"features"`
				Record   struct {
					Store string `json:"store"`
					Dir   string `json:"dir"`
				} `json:"record"`
				Managed bool `json:"managed"`
			} `json:"_meta"`
		} `json:"agentCapabilities"`
	}
	r.cl.ok("initialize", map[string]any{"protocolVersion": 1}, &res)
	caps := res.AgentCapabilities
	m := caps.Meta[acpMetaKey]
	if m.APILevel != 1 || m.Edition != "ce" || m.Version != "1.2.3" || m.Commit != "abc" || m.Managed {
		t.Fatalf("handshake: %+v", m)
	}
	if m.Record.Store != "local" || !strings.HasPrefix(m.Record.Dir, r.home) {
		t.Fatalf("record: %+v", m.Record)
	}
	for _, f := range []string{"sessions", "fork", "events", "record.verify", "hawkeye", "export", "tasks", "tasks.review",
		"capabilities", "policy.explain", "trust.inspect", "modes", "review", "checkpoints", "terminal", "queue", "manual", "doctor", "mcp.restart"} {
		if !slices.Contains(m.Features, f) {
			t.Errorf("feature %s not advertised: %v", f, m.Features)
		}
	}
	// Not served, so not listed: Studio hides them and never probes.
	for _, f := range []string{"resolve", "index", "infra", "memory", "team"} {
		if slices.Contains(m.Features, f) {
			t.Errorf("feature %s advertised but not served", f)
		}
	}
	if !caps.LoadSession || caps.SessionCapabilities["list"] == nil || caps.SessionCapabilities["delete"] != nil {
		t.Fatalf("spec capabilities: load %v, %v", caps.LoadSession, caps.SessionCapabilities)
	}
	r.cl.refused(errNoMethod, "session/delete", map[string]any{"sessionId": "x"})
	r.cl.refused(errNoMethod, "_abhed/team/status", map[string]any{})
}
