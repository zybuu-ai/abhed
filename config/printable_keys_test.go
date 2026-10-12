package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// controlBytes are what a config file's keys must never put on a terminal.
var controlBytes = []string{"\x00", "\x1b", "\x07"}

func assertVisible(t *testing.T, what, s string) {
	t.Helper()
	for _, c := range controlBytes {
		if strings.Contains(s, c) {
			t.Errorf("%s prints %q as it is:\n%q", what, c, s)
		}
	}
}

// An unknown key holding NUL, ESC or BEL is warned about in visible form.
func TestUnknownKeyWarningShowsControlBytes(t *testing.T) {
	t.Setenv(TrustEnv, "1")
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	path := filepath.Join(ws, ".abhed", "config.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	body := `{"a\u0000b": 1, "c\u001b[2Jd": 2, "e\u0007f": 3, "sandbox": {"g\u001b]0;x\u0007": true}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var warned bytes.Buffer
	warnOut = &warned
	t.Cleanup(func() { warnOut = os.Stderr })
	cfg, _ := Load(ws)
	if len(cfg.Unknown) != 4 {
		t.Fatalf("unknown keys %v, want 4", cfg.Unknown)
	}
	assertVisible(t, "the warning", warned.String())
	for _, want := range []string{`a⟨\0⟩b`, `c⟨\e⟩[2Jd`, `e⟨\a⟩f`, `sandbox.g⟨\e⟩]0;x⟨\a⟩`} {
		if !strings.Contains(warned.String(), want) {
			t.Errorf("the warning does not show %s:\n%s", want, warned.String())
		}
	}
	for _, u := range cfg.Unknown {
		assertVisible(t, "UnknownKey.String", u.String())
	}
}

// The managed file's refusals and errors show a key's control bytes too.
func TestManagedRefusalsShowControlBytes(t *testing.T) {
	err := CheckManagedJSON([]byte(`{"x\u0000\u001b\u0007": {"k\u001b": 1, "k\u001b": 2}}`))
	if err == nil {
		t.Fatal("a repeated key was taken")
	}
	assertVisible(t, "CheckManagedJSON", err.Error())
	err = CheckManagedJSON([]byte(`{"k\u0007\u0000": 1, "k\u0007\u0000": 2}`))
	if err == nil || !strings.Contains(err.Error(), `k⟨\a⟩⟨\0⟩`) {
		t.Fatalf("the repeated key is not named visibly: %v", err)
	}
	me := (&ManagedError{Key: "a\x1b[31m", Value: "v\x00\x07", Reason: "r", File: "/etc/abhed/config.json"}).Error()
	assertVisible(t, "ManagedError", me)

	w := WorkspaceTrust{File: "/w/.abhed/config.json", Ignored: []IgnoredKey{{Key: "model.providers.p\x1b]0;t\x07.base_url"}}}
	assertVisible(t, "the untrusted warning", w.Warning())
}

// A never-matching allow rule from a file is warned about visibly.
func TestNeverAllowsWarningShowsControlBytes(t *testing.T) {
	var warned bytes.Buffer
	warnOut = &warned
	t.Cleanup(func() { warnOut = os.Stderr })
	warnNeverAllows(nil, []string{"bash(a\x1b[2J && b\x07\x00)"})
	if warned.Len() == 0 {
		t.Fatal("no warning")
	}
	assertVisible(t, "the never-matches warning", warned.String())
}
