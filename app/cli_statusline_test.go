package app

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func TestSanitizeStatus(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":                "plain text",
		"\x1b[1;32mgreen\x1b[0m":    "\x1b[1;32mgreen\x1b[0m\x1b[0m",
		"\x1b]0;pwned\x07after":     "after",
		"\x1b]52;c;ZXZpbA==\x1b\\x": "x",
		"a\x1b[2Jb\x1b[10;1Hc":      "abc",
		"bell\x07 and\rcr":          "bell andcr",
		"tab\there":                 "tab here",
		"\x1b7save":                 "save",
		strings.Repeat("x", 300):    strings.Repeat("x", 200),
		"unicode ▲ ok":              "unicode ▲ ok",
	} {
		if got := sanitizeStatus(in); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
}

func testSandbox(t *testing.T, ws string) *lazySandbox {
	t.Helper()
	cfg := config.Default()
	cfg.Sandbox.MinTier = "none"
	sb, err := startSandbox(cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	sb.wait()
	return sb
}

func TestRunStatusline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	sb := testSandbox(t, ws)
	m := ui.StatusModel{Provider: "stub", Mode: "plan"}
	got, err := runStatusline(context.Background(), sb, ws, `grep -o '"mode":"[a-z]*"'; printf '\033]0;title\007'`, m)
	if err != nil || got != `"mode":"plan"` {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := runStatusline(context.Background(), sb, ws, "sleep 2", m); err == nil || !strings.Contains(err.Error(), "300 ms") {
		t.Fatalf("slow command: %v", err)
	}
	if got, err := runStatusline(context.Background(), sb, ws, "", m); got != "" || err != nil {
		t.Fatalf("no command: %q %v", got, err)
	}
}
