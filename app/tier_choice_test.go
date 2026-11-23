package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// /config set sandbox.tier writes the person's choice, asks before a chosen
// tier, and refuses one below the minimum in force, the managed one too.
func TestConfigSetSandboxTier(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	file := filepath.Join(home, ".abhed", "config.json")

	cfg := config.Default()
	cfg.Sandbox.MinTier = "container"
	cfg.ManagedKeys = []string{"sandbox.min_tier"}
	e, s := configEnv(cfg, ui.ChoiceYes)
	err := configSet(context.Background(), e, "sandbox.tier", "process")
	if err == nil || !strings.Contains(err.Error(), "weaker than the minimum tier in force, container (sandbox.min_tier, from managed)") || s.dialogs != 0 {
		t.Fatalf("under a managed container minimum: %v, %d dialogs", err, s.dialogs)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("written though refused")
	}
	if err := configSet(context.Background(), e, "sandbox.tier", "outer"); err == nil {
		t.Fatal("an unknown tier was taken")
	}
	// A managed tier is the administrator's.
	m := config.Default()
	m.ManagedKeys = []string{"sandbox.tier"}
	e, _ = configEnv(m, ui.ChoiceYes)
	if err := configSet(context.Background(), e, "sandbox.tier", "vm"); err == nil || !strings.Contains(err.Error(), "managed") {
		t.Fatalf("a managed tier: %v", err)
	}

	// Choosing a tier asks; no answer writes nothing.
	e, s = configEnv(config.Default(), "")
	if err := configSet(context.Background(), e, "sandbox.tier", "process"); err == nil || s.dialogs != 1 {
		t.Fatalf("no answer: %v, %d dialogs", err, s.dialogs)
	}
	e, _ = configEnv(config.Default(), ui.ChoiceYes)
	if err := configSet(context.Background(), e, "sandbox.tier", "process"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), `"tier": "process"`) {
		t.Fatalf("file: %s", b)
	}
	// Back to auto, or to a stronger tier, narrows: no question.
	own := config.Default()
	own.Sandbox.Tier = "process"
	for _, v := range []string{"container", "auto"} {
		e, s = configEnv(own, "")
		if err := configSet(context.Background(), e, "sandbox.tier", v); err != nil || s.dialogs != 0 {
			t.Fatalf("%s: %v, %d dialogs", v, err, s.dialogs)
		}
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), `"tier": ""`) {
		t.Fatalf("auto: %s", b)
	}
}

// The process tier is preferred only on a Mac, with no tier chosen and
// nothing managed setting the tier or its minimum.
func TestPreferOwnToolsWhere(t *testing.T) {
	c := config.Default()
	if !sandboxconfig.PreferOwnTools(c, "darwin") || sandboxconfig.PreferOwnTools(c, "linux") {
		t.Fatal("darwin only")
	}
	chosen := c
	chosen.Sandbox.Tier = "container"
	managedMin := c
	managedMin.ManagedKeys = []string{"sandbox.min_tier"}
	managedTier := c
	managedTier.ManagedKeys = []string{"sandbox.tier"}
	for name, cc := range map[string]config.Config{"chosen": chosen, "managed min": managedMin, "managed tier": managedTier} {
		if sandboxconfig.PreferOwnTools(cc, "darwin") {
			t.Errorf("%s: preferred", name)
		}
	}
}

// Doctor says why the tier was chosen and how to choose another, within
// the minimum.
func TestTierChoiceLines(t *testing.T) {
	sb, err := sandbox.Select(sandbox.Policy{Workspace: t.TempDir(), MinTier: sandbox.TierNone, Tier: sandbox.TierProcess})
	if err != nil {
		t.Skip("the process tier is not available here: " + err.Error())
	}
	c := config.Default()
	c.Sandbox.Tier = "process"
	got := strings.Join(tierChoiceLines(c, sb), "\n")
	if !strings.Contains(got, "chosen: chosen by sandbox.tier (process)") || strings.Contains(got, "/config set") {
		t.Fatalf("chosen:\n%s", got)
	}
	c = config.Default()
	c.Sandbox.MinTier = "container"
	c.ManagedKeys = []string{"sandbox.min_tier"}
	got = strings.Join(tierChoiceLines(c, sb), "\n")
	if !strings.Contains(got, "no weaker than container, set by the managed configuration") {
		t.Fatalf("auto:\n%s", got)
	}
}
