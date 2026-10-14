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
	if got := gitHeldLabel(cfg, sandbox.TierNone); !strings.Contains(got, "refused") {
		t.Errorf("required on none: %q", got)
	}
	off := false
	cfg.Sandbox.GitProtect = &off
	if got := gitHeldLabel(cfg, sandbox.TierProcess); !strings.HasPrefix(got, "not held: sandbox.protect_git is false") {
		t.Errorf("off: %q", got)
	}
}
