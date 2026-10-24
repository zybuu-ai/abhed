package app

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

type warnAgent struct {
	warnings []string
	trust    config.WorkspaceTrust
}

func (w warnAgent) ConfigWarnings() []string              { return w.warnings }
func (w warnAgent) WorkspaceTrust() config.WorkspaceTrust { return w.trust }

// session/new tells the editor what is wrong with the configuration: repeated
// and unread keys, and what an untrusted workspace file could not set.
func TestACPConfigWarningsListRepeatedAndIgnoredKeys(t *testing.T) {
	a := warnAgent{
		warnings: []string{`/h/.abhed/config.json: the key "gemini" appears more than once in model.providers; only the last one is used`},
		trust:    config.WorkspaceTrust{File: "/w/.abhed/config.json", Ignored: []config.IgnoredKey{{Key: "model.providers.mine"}}},
	}
	got := configWarnings(a)
	if len(got) != 2 || !strings.Contains(got[0], "appears more than once") ||
		got[1] != "/w/.abhed/config.json: model.providers.mine is not used while the workspace configuration is untrusted" {
		t.Fatalf("configWarnings = %q", got)
	}
	a.trust.Trusted = true
	if got := configWarnings(a); len(got) != 1 {
		t.Fatalf("a trusted file's keys are not ignored: %q", got)
	}
	if got := configWarnings(struct{}{}); got != nil {
		t.Fatalf("an agent that reports nothing: %q", got)
	}
}
