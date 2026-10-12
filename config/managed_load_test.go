package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/managed"
)

// Something at the managed path that cannot be read stops loading: treating
// it as absent would run unmanaged exactly when the organisation meant not to.
func TestUnreadableManagedFileIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	path := withManaged(t, `{"permissions": {"mode": "default"}}`)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := Load(t.TempDir()); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("Load: %v", err)
	}
	if _, err := LoadManaged(); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("LoadManaged: %v", err)
	}

	// A directory that cannot be searched hides the file from stat as well.
	_ = os.Chmod(path, 0o600)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := Load(t.TempDir()); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("Load, unsearchable directory: %v", err)
	}
	if _, err := LoadManaged(); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("LoadManaged, unsearchable directory: %v", err)
	}
}

func TestDanglingManagedLinkIsAnError(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(filepath.Join(dir, "gone.json"), link); err != nil {
		t.Skip(err)
	}
	old := managed.ConfigFile
	managed.ConfigFile = link
	t.Cleanup(func() { managed.ConfigFile = old })
	t.Setenv("HOME", t.TempDir())
	if _, err := Load(t.TempDir()); err == nil || !strings.Contains(err.Error(), "link to nothing") {
		t.Errorf("Load: %v", err)
	}
	if _, err := LoadManaged(); err == nil || !strings.Contains(err.Error(), "link to nothing") {
		t.Errorf("LoadManaged: %v", err)
	}
}

// Keys fold as encoding/json folds them, so a key the decoder applies is
// recorded, and not reported as unknown.
func TestManagedKeysFoldAsTheDecoderDoes(t *testing.T) {
	withManaged(t, `{"ſandbox": {"min_tier": "none"}}`)
	var warned strings.Builder
	warnOut = &warned
	t.Cleanup(func() { warnOut = os.Stderr })
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.MinTier != "none" || !cfg.ManagedSets("sandbox.min_tier") {
		t.Errorf("min_tier %q, keys %v", cfg.Sandbox.MinTier, cfg.ManagedKeys)
	}
	if len(cfg.Unknown) > 0 || warned.Len() > 0 {
		t.Errorf("an applied key was reported unknown: %v %s", cfg.Unknown, warned.String())
	}
}

func TestApplyRefusesUnknownModes(t *testing.T) {
	for _, c := range []Config{Default(), managedCfg()} {
		for _, m := range []string{"BYPASS", "Auto", "paln"} {
			if _, err := c.Apply(Overrides{Mode: m}); err == nil || !strings.Contains(err.Error(), "unknown permission mode") {
				t.Errorf("%q: %v", m, err)
			}
		}
	}
}

func TestManagedErrorNamesTheFile(t *testing.T) {
	path := withManaged(t, `{"permissions": {"mode": "default"}}`)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Apply(Overrides{Mode: "auto"}); err == nil || !strings.Contains(err.Error(), "set in "+path) {
		t.Errorf("the refusal does not name the managed file: %v", err)
	}
}

// A managed max_turns of zero or less binds nothing.
func TestManagedZeroMaxTurnsBindsNothing(t *testing.T) {
	c := managedCfg("limits.max_turns")
	c.Limits.MaxTurns = 0
	if got, err := c.Apply(Overrides{MaxTurns: 1000}); err != nil || got.Limits.MaxTurns != 1000 {
		t.Errorf("%v %d", err, got.Limits.MaxTurns)
	}
}

// /etc/abhed itself a link to nothing is an error, not an unmanaged run; a
// folder that is simply missing is unmanaged.
func TestDanglingManagedFolderIsAnError(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "abhed")
	if err := os.Symlink(filepath.Join(dir, "gone"), folder); err != nil {
		t.Skip(err)
	}
	old := managed.ConfigFile
	t.Cleanup(func() { managed.ConfigFile = old })
	t.Setenv("HOME", t.TempDir())
	managed.ConfigFile = filepath.Join(folder, "config.json")
	if _, err := Load(t.TempDir()); err == nil || !strings.Contains(err.Error(), "folder "+folder+" is a link to nothing") {
		t.Errorf("Load: %v", err)
	}
	managed.ConfigFile = filepath.Join(dir, "missing", "config.json")
	if cfg, err := Load(t.TempDir()); err != nil || cfg.Managed {
		t.Errorf("a missing folder: managed %v, %v", cfg.Managed, err)
	}
}

// LoadOptions.Warn takes a load's warnings, every time, and nothing reaches
// stderr; without it each goes to stderr once per process.
func TestLoadWarnTakesTheWarnings(t *testing.T) {
	old := managed.ConfigFile
	managed.ConfigFile = filepath.Join(t.TempDir(), "none.json")
	t.Cleanup(func() { managed.ConfigFile = old })
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(`{"no_such_key_g20":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	warnOut = &stderr
	t.Cleanup(func() { warnOut = os.Stderr })
	for i := 0; i < 2; i++ {
		var got []string
		if _, err := LoadWith(t.TempDir(), LoadOptions{Warn: func(s string) { got = append(got, s) }}); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !strings.Contains(got[0], "no_such_key_g20") {
			t.Fatalf("load %d: warnings %q", i, got)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr got %q", stderr.String())
	}
	if _, err := Load(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "no_such_key_g20") {
		t.Fatalf("without Warn, nothing on stderr: %q", stderr.String())
	}
}
