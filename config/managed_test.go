package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

// Wake defaults to auto, and a managed notify holds over a user's auto.
func TestManagedWakeHoldsOverUser(t *testing.T) {
	withManaged(t, `{}`)
	if cfg, err := Load(t.TempDir()); err != nil || cfg.Subagents.Wake != "auto" {
		t.Fatalf("default wake %q, %v", cfg.Subagents.Wake, err)
	}
	withManaged(t, `{"subagents": {"wake": "notify"}}`)
	writeConfig(t, os.Getenv("HOME"), `{"subagents": {"wake": "auto"}}`)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Subagents.Wake != "notify" || !cfg.ManagedSets("subagents.wake") {
		t.Fatalf("wake %q with a managed notify", cfg.Subagents.Wake)
	}
}

// A managed file that sets any permissions key, not only the allow list,
// leaves no caller able to add an allow rule.
func TestAllowRefusedUnderAnyManagedPermission(t *testing.T) {
	for _, key := range []string{"permissions.mode", "permissions.deny", "permissions.ask", "permissions"} {
		c := managedCfg(key)
		if !c.AllowLocked() {
			t.Errorf("%s: allow rules not locked", key)
		}
		_, err := c.Apply(Overrides{Allow: []string{"bash(touch *)"}})
		refused(t, err, "permissions.allow")
	}
	c := managedCfg("limits.max_turns", "sandbox.min_tier")
	if got, err := c.Apply(Overrides{Allow: []string{"bash(touch *)"}}); err != nil || c.AllowLocked() ||
		!strings.Contains(strings.Join(got.Permissions.Allow, " "), "bash(touch *)") {
		t.Errorf("a managed file without permissions refused an allow rule: %v", err)
	}
}

// Under a managed file that sets any permissions setting but not
// permissions.allow, the allow rules the user's and a trusted workspace's
// files add are left out and each is named; the defaults stay.
func TestManagedLockDropsFileAllowRules(t *testing.T) {
	for name, body := range map[string]string{
		"deny only": `{"permissions":{"deny":["bash(curl*)"]}}`,
		"mode only": `{"permissions":{"mode":"default"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			withManaged(t, body)
			home, ws := trustHome(t, `{"permissions":{"allow":["bash(rm*)"]}}`,
				`{"permissions":{"allow":["bash(rm*)","bash(*)"]}}`)
			var warned bytes.Buffer
			warnOut = &warned
			t.Cleanup(func() { warnOut = os.Stderr })
			cfg, err := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.AllowLocked() || !cfg.Workspace.Trusted {
				t.Fatalf("locked %v trusted %v", cfg.AllowLocked(), cfg.Workspace.Trusted)
			}
			if len(cfg.Permissions.Allow) != 0 || cfg.Sets("permissions.allow") {
				t.Fatalf("file allow rules survived the lock: %v", cfg.Permissions.Allow)
			}
			userFile := filepath.Join(home, ".abhed", "config.json")
			for rule, file := range map[string]string{"bash(rm*)": userFile, "bash(*)": cfg.Workspace.File} {
				if !slices.ContainsFunc(cfg.SetAside, func(k SetAsideKey) bool {
					return k.Key == "permissions.allow" && k.Value == rule && k.File == file
				}) {
					t.Errorf("%s from %s not set aside: %+v", rule, file, cfg.SetAside)
				}
				if want := file + " sets permissions.allow " + rule; !strings.Contains(warned.String(), want) {
					t.Errorf("no warning %q:\n%s", want, warned.String())
				}
			}
		})
	}

	t.Run("defaults stay", func(t *testing.T) {
		withManaged(t, `{"permissions":{"deny":["bash(curl*)"]}}`)
		_, ws := trustHome(t, "", "")
		cfg, err := Load(ws)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cfg.Permissions.Allow, Default().Permissions.Allow) || len(cfg.SetAside) != 0 {
			t.Fatalf("allow %v, aside %v; want the defaults and nothing set aside", cfg.Permissions.Allow, cfg.SetAside)
		}
	})

	t.Run("managed allow list", func(t *testing.T) {
		withManaged(t, `{"permissions":{"deny":["bash(curl*)"],"allow":["bash(go test*)"]}}`)
		_, ws := trustHome(t, `{"permissions":{"allow":["bash(rm*)"]}}`, `{"permissions":{"allow":["bash(*)"]}}`)
		cfg, err := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cfg.Permissions.Allow, []string{"bash(go test*)"}) || len(cfg.SetAside) != 0 {
			t.Fatalf("allow %v, aside %v; want exactly the managed list", cfg.Permissions.Allow, cfg.SetAside)
		}
	})

	t.Run("no managed file", func(t *testing.T) {
		old := managed.ConfigFile
		managed.ConfigFile = filepath.Join(t.TempDir(), "absent.json")
		t.Cleanup(func() { managed.ConfigFile = old })
		_, ws := trustHome(t, `{"permissions":{"allow":["bash(rm*)"]}}`, `{"permissions":{"allow":["bash(rm*)","bash(*)"]}}`)
		cfg, err := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cfg.Permissions.Allow, []string{"bash(rm*)", "bash(*)"}) || len(cfg.SetAside) != 0 {
			t.Fatalf("allow %v, aside %v; want the files' rules unchanged", cfg.Permissions.Allow, cfg.SetAside)
		}
	})
}
