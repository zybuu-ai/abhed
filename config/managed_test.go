package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/managed"
)

// withManaged points the managed path at a file holding body for one test,
// and returns that path.
func withManaged(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
	t.Setenv("HOME", t.TempDir())
	return path
}

// Load records each setting the managed file makes, and nothing it does not.
func TestLoadRecordsWhatTheManagedFileSets(t *testing.T) {
	withManaged(t, `{
	  "Permissions": {"mode": "default", "deny": ["bash(curl*)"]},
	  "tools": {"syntax_check": "refuse"},
	  "model": {"providers": {"local": {"type": "ollama", "base_url": "http://gpu:8000/v1", "model": "m"}}},
	  "sandbox": {},
	  "limits": {"max_turns": null},
	  "not_a_key": 1,
	  "_comment": "for people"
	}`)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"model.providers.local", "permissions.deny", "permissions.mode", "tools.syntax_check"}
	if !cfg.Managed || !reflect.DeepEqual(cfg.ManagedKeys, want) {
		t.Fatalf("managed %v, keys %v; want %v", cfg.Managed, cfg.ManagedKeys, want)
	}
	for path, set := range map[string]bool{
		"permissions.mode": true, "permissions": true, "model.providers.local.model": true,
		"permissions.allow": false, "sandbox": false, "limits.max_turns": false, "tools.syntax": false,
	} {
		if got := cfg.ManagedSets(path); got != set {
			t.Errorf("ManagedSets(%q) = %v, want %v", path, got, set)
		}
	}
	if cfg.Permissions.Deny[0] != "bash(curl*)" {
		t.Errorf("the managed file was not applied: %v", cfg.Permissions.Deny)
	}
}

func TestNoManagedFileRecordsNothing(t *testing.T) {
	old := managed.ConfigFile
	managed.ConfigFile = filepath.Join(t.TempDir(), "absent.json")
	t.Cleanup(func() { managed.ConfigFile = old })
	t.Setenv("HOME", t.TempDir())
	for _, load := range []func() (Config, error){
		func() (Config, error) { return Load(t.TempDir()) }, LoadManaged,
	} {
		cfg, err := load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Managed || len(cfg.ManagedKeys) > 0 || cfg.ManagedSets("permissions.mode") {
			t.Errorf("no managed file, yet managed %v keys %v", cfg.Managed, cfg.ManagedKeys)
		}
	}
}

// LoadManaged is the defaults plus the managed file, and no user or project file.
func TestLoadManagedReadsOnlyTheManagedFile(t *testing.T) {
	withManaged(t, `{"permissions": {"mode": "plan"}}`)
	home := os.Getenv("HOME")
	_ = os.MkdirAll(filepath.Join(home, ".abhed"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(`{"tools":{"syntax_check":"off"}}`), 0o644)
	cfg, err := LoadManaged()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Permissions.Mode != "plan" || !cfg.Managed || cfg.Tools.SyntaxCheck != "" {
		t.Errorf("mode %q managed %v syntax %q", cfg.Permissions.Mode, cfg.Managed, cfg.Tools.SyntaxCheck)
	}
}

func managedCfg(keys ...string) Config {
	c := Default()
	c.Managed = true
	c.ManagedKeys = keys
	return c
}

func refused(t *testing.T, err error, key string) {
	t.Helper()
	var me *ManagedError
	if !errors.As(err, &me) || me.Key != key {
		t.Fatalf("want a refusal on %s, got %v", key, err)
	}
}

func TestApplyUnderManagedTightensOnly(t *testing.T) {
	c := managedCfg()
	_, err := c.Apply(Overrides{Mode: "bypass"})
	refused(t, err, "permissions.mode")
	if got, err := c.Apply(Overrides{Mode: "auto"}); err != nil || got.Permissions.Mode != "auto" {
		t.Errorf("an unpinned mode other than bypass may be chosen: %v %q", err, got.Permissions.Mode)
	}

	c = managedCfg("permissions.mode")
	c.Permissions.Mode = "default"
	for _, m := range []string{"auto", "accept-edits", "bypass"} {
		_, err := c.Apply(Overrides{Mode: m})
		refused(t, err, "permissions.mode")
	}
	for _, m := range []string{"", "default", "plan"} {
		if _, err := c.Apply(Overrides{Mode: m}); err != nil {
			t.Errorf("%q: %v", m, err)
		}
	}

	c = managedCfg("tools.syntax_check")
	c.Tools.SyntaxCheck = "report"
	_, err = c.Apply(Overrides{SyntaxCheck: "off"})
	refused(t, err, "tools.syntax_check")
	if got, err := c.Apply(Overrides{SyntaxCheck: "refuse"}); err != nil || got.Tools.SyntaxCheck != "refuse" {
		t.Errorf("a stricter syntax check is allowed: %v", err)
	}
	c.Tools.SyntaxCheck = "" // written as the default
	_, err = c.Apply(Overrides{SyntaxCheck: "report"})
	refused(t, err, "tools.syntax_check")

	c = managedCfg("limits.max_turns")
	c.Limits.MaxTurns = 10
	_, err = c.Apply(Overrides{MaxTurns: 11})
	refused(t, err, "limits.max_turns")
	if got, _ := c.Apply(Overrides{MaxTurns: 5}); got.Limits.MaxTurns != 5 {
		t.Error("a lower turn limit is allowed")
	}

	c = managedCfg("permissions.allow", "additional_dirs")
	_, err = c.Apply(Overrides{Allow: []string{"bash(*)"}})
	refused(t, err, "permissions.allow")
	_, err = c.Apply(Overrides{AdditionalDirs: []string{"/"}})
	refused(t, err, "additional_dirs")
}

// Deny rules from a caller are added to the managed ones, never replace them.
func TestApplyAddsDenyRules(t *testing.T) {
	c := managedCfg("permissions.deny")
	c.Permissions.Deny = []string{"bash(curl*)"}
	got, err := c.Apply(Overrides{Deny: []string{"bash(wget*)"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Permissions.Deny, []string{"bash(curl*)", "bash(wget*)"}) {
		t.Errorf("deny = %v", got.Permissions.Deny)
	}
	if len(c.Permissions.Deny) != 1 {
		t.Error("Apply changed the configuration it was given")
	}
}

// Without a managed file every override applies, as before.
func TestApplyWithoutManagedIsUnchanged(t *testing.T) {
	c := Default()
	got, err := c.Apply(Overrides{Mode: "bypass", SyntaxCheck: "off", MaxTurns: 1000,
		Allow: []string{"bash(*)"}, AdditionalDirs: []string{"/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Permissions.Mode != "bypass" || got.Tools.SyntaxCheck != "off" || got.Limits.MaxTurns != 1000 ||
		!strings.Contains(strings.Join(got.Permissions.Allow, " "), "bash(*)") || got.AdditionalDirs[0] != "/tmp" {
		t.Errorf("overrides not applied: %+v", got.Permissions)
	}
}
