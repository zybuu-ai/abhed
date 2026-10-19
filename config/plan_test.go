package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zybuu-ai/abhed/internal/managed"
)

// Runbooks are on by default; an untrusted workspace may turn them off, not
// on, and the managed file binds either way.
func TestPlanRunbooksSwitch(t *testing.T) {
	if !Default().Plan.Runbooks {
		t.Fatal("plan.runbooks is off by default")
	}
	old := managed.ConfigFile
	t.Cleanup(func() { managed.ConfigFile = old })
	managed.ConfigFile = filepath.Join(t.TempDir(), "none.json")

	_, ws := trustHome(t, "", `{"plan":{"runbooks":false}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Plan.Runbooks || !slices.Contains(cfg.Workspace.Applied, "plan.runbooks") {
		t.Fatalf("the workspace could not turn runbooks off: %+v", cfg.Workspace)
	}

	_, ws = trustHome(t, `{"plan":{"runbooks":false}}`, `{"plan":{"runbooks":true}}`)
	if cfg, _ = LoadWith(ws, LoadOptions{Quiet: true}); cfg.Plan.Runbooks {
		t.Fatal("an untrusted workspace turned runbooks back on")
	}

	for _, c := range []struct {
		user, managed string
		want          bool
	}{
		{`{"plan":{"runbooks":false}}`, `{"plan":{"runbooks":true}}`, true},
		{`{"plan":{"runbooks":true}}`, `{"plan":{"runbooks":false}}`, false},
	} {
		path := filepath.Join(t.TempDir(), "managed.json")
		if err := os.WriteFile(path, []byte(c.managed), 0o600); err != nil {
			t.Fatal(err)
		}
		managed.ConfigFile = path
		_, ws := trustHome(t, c.user, "")
		cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
		if err != nil || cfg.Plan.Runbooks != c.want {
			t.Fatalf("managed %s over user %s: %v %v", c.managed, c.user, cfg.Plan.Runbooks, err)
		}
	}
}

// A rule only the managed file lists is the one a runbook does not answer.
func TestManagedAsk(t *testing.T) {
	old := managed.ConfigFile
	t.Cleanup(func() { managed.ConfigFile = old })
	path := filepath.Join(t.TempDir(), "managed.json")
	if err := os.WriteFile(path, []byte(`{"permissions":{"ask":["ssh(prod*/*)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	managed.ConfigFile = path
	_, ws := trustHome(t, `{"permissions":{"ask":["ssh(dev1/*)"]}}`, "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ManagedAsk("ssh(prod*/*)") || cfg.ManagedAsk("ssh(dev1/*)") {
		t.Fatalf("managed ask: %v %v (ask %v)", cfg.ManagedAsk("ssh(prod*/*)"), cfg.ManagedAsk("ssh(dev1/*)"), cfg.Permissions.Ask)
	}
}
