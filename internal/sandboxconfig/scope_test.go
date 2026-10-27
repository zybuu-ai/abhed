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

// Commands listen on loopback from the start only under sandbox.listen
// allow; under ask the person's agreement turns it on later.
func TestListenReachesThePolicy(t *testing.T) {
	cfg := config.Default()
	if p, err := Policy(cfg, t.TempDir()); err != nil || p.ListenLoopback {
		t.Fatalf("default: %v %v", p.ListenLoopback, err)
	}
	cfg.Sandbox.Listen = "allow"
	if p, err := Policy(cfg, t.TempDir()); err != nil || !p.ListenLoopback {
		t.Fatalf("allow: %v %v", p.ListenLoopback, err)
	}
}
