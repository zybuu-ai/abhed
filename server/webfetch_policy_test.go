package server

import (
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// A server session's policy asks before web_fetch when no host list is set.
func TestServerPolicyAsksBeforeWebFetchWithoutAHostList(t *testing.T) {
	cfg := config.Default()
	cfg.WebFetch.Enabled = true
	s := &Server{opts: Options{Config: cfg, Workspace: t.TempDir()}}
	args := []byte(`{"url":"https://example.com/"}`)
	// Plan too: a console client may narrow any session to plan, and that must
	// not turn an ask into a silent request.
	for _, mode := range []policy.Mode{policy.ModeAuto, policy.ModeDefault, policy.ModePlan} {
		if got := s.newPolicy(mode).Evaluate("web_fetch", false, args); got.Decision != policy.Ask {
			t.Fatalf("%s, no host list: %+v", mode, got)
		}
	}
	s.opts.Config.WebFetch.AllowedHosts = []string{"example.com"}
	if got := s.newPolicy(policy.ModeAuto).Evaluate("web_fetch", false, args); got.Decision != policy.Allow {
		t.Fatalf("with a host list: %+v", got)
	}
}
