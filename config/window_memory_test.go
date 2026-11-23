package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A limit kept is recalled by another WindowMemory, as a later process would;
// it lapses after LearnedWindowTTL, and a linked file is never read.
func TestWindowMemory(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	m := &WindowMemory{Path: filepath.Join(dir, "context-windows.json"), Now: func() time.Time { return now }}
	m.Remember("anthropic https://api.anthropic.com claude-sonnet-5-5", 200_000)
	later := &WindowMemory{Path: m.Path, Now: func() time.Time { return now.Add(24 * time.Hour) }}
	if got := later.Recall("anthropic https://api.anthropic.com claude-sonnet-5-5"); got != 200_000 {
		t.Fatalf("recalled %d", got)
	}
	if got := later.Recall("anthropic https://api.anthropic.com claude-opus-5-5"); got != 0 {
		t.Fatalf("another model recalled %d", got)
	}
	if info, err := os.Stat(m.Path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v %v", info, err)
	}
	stale := &WindowMemory{Path: m.Path, Now: func() time.Time { return now.Add(LearnedWindowTTL + time.Hour) }}
	if got := stale.Recall("anthropic https://api.anthropic.com claude-sonnet-5-5"); got != 0 {
		t.Fatalf("a lapsed limit recalled %d", got)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Link(m.Path, link); err != nil {
		t.Skip("no hard links here")
	}
	if got := (&WindowMemory{Path: link, Now: later.Now}).Recall("anthropic https://api.anthropic.com claude-sonnet-5-5"); got != 0 {
		t.Fatal("a linked file was read")
	}
}
