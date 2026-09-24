package app

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
)

// managedConfig points the managed path at a file holding body for one test.
// An empty body means no managed file at all.
func managedConfig(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
	t.Setenv("HOME", t.TempDir())
}

func loadFlags(t *testing.T, mode string, turns int, allow, deny, dirs string) (config.Config, error) {
	t.Helper()
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return applyFlags(cfg, mode, turns, allow, deny, dirs)
}

// The command line's flags meet the managed configuration as the SDK's Options do.
func TestManagedConfigBindsTheFlags(t *testing.T) {
	managedConfig(t, `{"permissions": {"mode": "default", "allow": ["bash(go test*)"], "deny": ["bash(curl*)"]},
	  "limits": {"max_turns": 10}, "additional_dirs": []}`)
	for name, c := range map[string]struct {
		mode, allow, dirs string
		turns             int
		key               string
	}{
		"mode":      {mode: "auto", key: "permissions.mode"},
		"bypass":    {mode: "bypass", key: "permissions.mode"},
		"allow":     {allow: "bash(*)", key: "permissions.allow"},
		"max turns": {turns: 11, key: "limits.max_turns"},
		"add dir":   {dirs: "/", key: "additional_dirs"},
	} {
		_, err := loadFlags(t, c.mode, c.turns, c.allow, "", c.dirs)
		var me *config.ManagedError
		if !errors.As(err, &me) || me.Key != c.key {
			t.Errorf("%s: want %s refused, got %v", name, c.key, err)
		}
	}
	cfg, err := loadFlags(t, "plan", 5, "", "bash(wget*)", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Permissions.Mode != "plan" || cfg.Limits.MaxTurns != 5 ||
		!slices.Equal(cfg.Permissions.Deny, []string{"bash(curl*)", "bash(wget*)"}) {
		t.Errorf("tightening flags not applied: mode %q turns %d deny %v",
			cfg.Permissions.Mode, cfg.Limits.MaxTurns, cfg.Permissions.Deny)
	}
}

func TestFlagsWithoutManagedConfigAreUnchanged(t *testing.T) {
	managedConfig(t, "")
	cfg, err := loadFlags(t, "bypass", 500, "bash(*)", "bash(wget*)", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Permissions.Mode != "bypass" || cfg.Limits.MaxTurns != 500 ||
		!slices.Contains(cfg.Permissions.Allow, "bash(*)") || !slices.Contains(cfg.Permissions.Deny, "bash(wget*)") ||
		!slices.Equal(cfg.AdditionalDirs, []string{"/tmp"}) {
		t.Errorf("flags not applied: %+v %v", cfg.Permissions, cfg.AdditionalDirs)
	}
}

// An eval approves every prompt unattended, so any managed file refuses it,
// whether or not it pins a mode.
func TestEvalIsRefusedUnderAManagedFile(t *testing.T) {
	for _, body := range []string{`{"permissions": {"mode": "default"}}`, `{"permissions": {"deny": ["bash(curl*)"]}}`} {
		managedConfig(t, body)
		cfg, err := config.Load(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := evalAllowed(cfg); err == nil || !strings.Contains(err.Error(), "refused under the managed configuration "+managed.ConfigFile) {
			t.Errorf("%s: %v", body, err)
		}
		if code := evalCmd(t.TempDir(), t.TempDir(), ""); code == 0 {
			t.Errorf("%s: an eval ran under a managed file", body)
		}
	}
	managedConfig(t, "")
	if code := evalCmd(t.TempDir(), t.TempDir(), ""); code != 0 {
		t.Errorf("an empty corpus with no managed configuration exits %d", code)
	}
}
