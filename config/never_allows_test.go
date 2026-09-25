package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bash allow rule that can never match is named when the config loads, and
// the config still loads.
func TestAllowRuleThatNeverMatchesIsWarned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	path := filepath.Join(ws, ".abhed", "config.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(`{"permissions": {"allow": ["bash(cd x && go vet*)", "bash(go vet*)", "bash(*)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var warned bytes.Buffer
	warnOut = &warned
	t.Cleanup(func() { warnOut = os.Stderr })

	cfg, err := Load(ws)
	if err != nil && !strings.Contains(err.Error(), "model") {
		t.Fatalf("a config with a dead allow rule stopped loading: %v", err)
	}
	if len(cfg.Permissions.Allow) != 3 {
		t.Fatalf("allow rules: %v", cfg.Permissions.Allow)
	}
	out := warned.String()
	if !strings.Contains(out, "bash(cd x && go vet*) never matches") {
		t.Errorf("no warning for the chained allow rule: %q", out)
	}
	if strings.Contains(out, "bash(go vet*) never") || strings.Contains(out, "bash(*) never") {
		t.Errorf("a working allow rule was warned about: %q", out)
	}
}
