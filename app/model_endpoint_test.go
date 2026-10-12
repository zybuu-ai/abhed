package app

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress"
)

// Under the allowlist, doctor and serve -check warn of a model endpoint on an
// internal address, which the model client reaches though commands may not.
func TestInternalModelEndpointIsWarnedUnderTheAllowlist(t *testing.T) {
	cfg := config.Default()
	cfg.Model.Providers = map[string]config.ProviderConfig{
		"meta": {Type: "openai-compatible", BaseURL: "http://169.254.169.254/v1", Model: "m"},
		"pub":  {Type: "openai-compatible", BaseURL: "https://93.184.216.34/v1", Model: "m"},
	}
	if got := internalModelEndpoints(cfg); len(got) != 0 {
		t.Fatalf("warned without the allowlist: %v", got)
	}
	cfg.Sandbox.Network = config.NetworkAllowlist
	got := internalModelEndpoints(cfg)
	if len(got) != 1 || !strings.Contains(got[0], "provider meta") || !strings.Contains(got[0], "link-local") {
		t.Fatalf("warnings: %v", got)
	}
}

// Under the allowlist, doctor warns of a deny rule narrowed by a path.
func TestPathDenyIsWarned(t *testing.T) {
	cfg := config.Default()
	cfg.Sandbox.Network = config.NetworkAllowlist
	cfg.Egress.Rules = []egress.Rule{{Host: "api.example.com", Paths: []string{"/admin/*"}, Decision: "deny"},
		{Host: "api.example.com", Decision: "allow"}}
	if w := pathDenyWarning(cfg); !strings.Contains(w, "deny by path") {
		t.Fatalf("warning: %q", w)
	}
	cfg.Egress.Rules = cfg.Egress.Rules[1:]
	if w := pathDenyWarning(cfg); w != "" {
		t.Fatalf("warned with no path deny: %q", w)
	}
}
