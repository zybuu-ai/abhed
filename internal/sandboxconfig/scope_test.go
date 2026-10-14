package sandboxconfig

import (
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// The surface's ask to scope signals reaches the sandbox's policy.
func TestScopeSignalsReachesThePolicy(t *testing.T) {
	cfg := config.Default()
	cfg.Sandbox.ScopeSignals = true
	p, err := Policy(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !p.ScopeSignals {
		t.Fatal("ScopeSignals was dropped")
	}
}
