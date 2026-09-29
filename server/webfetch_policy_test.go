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
	if got := s.newPolicy(policy.ModeAuto).Evaluate("web_fetch", false, args); got.Decision != policy.Ask {
		t.Fatalf("no host list: %+v", got)
	}
	s.opts.Config.WebFetch.AllowedHosts = []string{"example.com"}
	if got := s.newPolicy(policy.ModeAuto).Evaluate("web_fetch", false, args); got.Decision != policy.Allow {
		t.Fatalf("with a host list: %+v", got)
	}
}
