package app

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// The git line says what the tier in force does: on none git's files are not
// held, and the line says so rather than promising they are.
func TestGitHeldLabelFollowsTheTier(t *testing.T) {
	var cfg config.Config
	if got := gitHeldLabel(cfg, sandbox.TierNone); !strings.HasPrefix(got, "not held") || !strings.Contains(got, "none") {
		t.Errorf("none: %q", got)
	}
	for _, tier := range []sandbox.Tier{sandbox.TierProcess, ""} {
		if got := gitHeldLabel(cfg, tier); !strings.HasPrefix(got, "held read-only from commands") {
			t.Errorf("%q: %q", tier, got)
		}
	}
	on := true
	cfg.Sandbox.GitProtect = &on
	// Required, on none: nothing holds them, and the line says so.
	if got := gitHeldLabel(cfg, sandbox.TierNone); !strings.HasPrefix(got, "not held") {
		t.Errorf("required on none: %q", got)
	}
	if got := gitHeldLabel(cfg, sandbox.TierProcess); !strings.Contains(got, "refused") {
		t.Errorf("required on process: %q", got)
	}
	// Serve asks for it on every tier; on none its banner says it is not held.
	served := config.Config{}
	served.Sandbox.ProtectGit = true
	if got := gitHeldLabel(served, sandbox.TierNone); !strings.HasPrefix(got, "not held") {
		t.Errorf("serve on none: %q", got)
	}
	off := false
	cfg.Sandbox.GitProtect = &off
	if got := gitHeldLabel(cfg, sandbox.TierProcess); !strings.HasPrefix(got, "not held: sandbox.protect_git is false") {
		t.Errorf("off: %q", got)
	}
}

// abhed serve with sandbox.protect_git set true in the configuration does not
// start on the none tier, which cannot hold git's files; without it, it does.
func TestServeRefusesProtectGitOnTheNoneTier(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no sandbox-exec, bubblewrap or engine: the none tier
	ws := t.TempDir()
	cfg := config.Default()
	cfg.Sandbox.MinTier = "none"
	on := true
	cfg.Sandbox.GitProtect = &on
	if _, _, _, err := serveSandbox(cfg, ws); err == nil || !strings.Contains(err.Error(), "sandbox.protect_git is true") {
		t.Fatalf("served on none with protect_git true: %v", err)
	}
	cfg.Sandbox.GitProtect = nil
	_, sb, _, err := serveSandbox(cfg, ws)
	if err != nil {
		t.Fatalf("without protect_git set: %v", err)
	}
	defer func() { _ = sandbox.Close(sb) }()
	if sb.Tier() != sandbox.TierNone {
		t.Fatalf("tier %s", sb.Tier())
	}
}
