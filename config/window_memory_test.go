package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
)

// A limit kept is recalled by another WindowMemory, as a later process would;
// it lapses after LearnedWindowTTL, and a linked file is never read.
func TestWindowMemory(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	m := &WindowMemory{Path: filepath.Join(dir, "context-windows.json"), Now: func() time.Time { return now }}
	m.Remember("anthropic https://api.anthropic.com claude-sonnet-5-5", 200_000)
	later := &WindowMemory{Path: m.Path, Now: func() time.Time { return now.Add(24 * time.Hour) }}
	if got, at := later.Recall("anthropic https://api.anthropic.com claude-sonnet-5-5"); got != 200_000 || !at.Equal(now) {
		t.Fatalf("recalled %d at %v", got, at)
	}
	if got, _ := later.Recall("anthropic https://api.anthropic.com claude-opus-5-5"); got != 0 {
		t.Fatalf("another model recalled %d", got)
	}
	if info, err := os.Stat(m.Path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v %v", info, err)
	}
	stale := &WindowMemory{Path: m.Path, Now: func() time.Time { return now.Add(LearnedWindowTTL + time.Hour) }}
	if got, _ := stale.Recall("anthropic https://api.anthropic.com claude-sonnet-5-5"); got != 0 {
		t.Fatalf("a lapsed limit recalled %d", got)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Link(m.Path, link); err != nil {
		t.Skip("no hard links here")
	}
	if got, _ := (&WindowMemory{Path: link, Now: later.Now}).Recall("anthropic https://api.anthropic.com claude-sonnet-5-5"); got != 0 {
		t.Fatal("a linked file was read")
	}
	// Nor written through: a linked file is left as it was.
	before, _ := os.ReadFile(link)
	if err := (&WindowMemory{Path: link, Now: later.Now}).change("other", 300_000); err == nil {
		t.Fatal("wrote through a linked file")
	}
	if after, _ := os.ReadFile(link); string(after) != string(before) {
		t.Fatal("the linked file changed")
	}
}

// Forget drops one key and keeps the rest.
func TestWindowMemoryForget(t *testing.T) {
	m := &WindowMemory{Path: filepath.Join(t.TempDir(), "w.json")}
	m.Remember("a", 200_000)
	m.Remember("b", 100_000)
	m.Forget("a")
	if w, _ := m.Recall("a"); w != 0 {
		t.Fatal("not forgotten")
	}
	if w, _ := m.Recall("b"); w != 100_000 {
		t.Fatal("another key was dropped")
	}
}

// A base URL carrying a user, a password and a key in its query leaves no
// trace of them in the file: keys are hashes, and nothing else is written.
func TestWindowMemoryFileHoldsNoCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context-windows.json")
	model.SetWindowMemory(&WindowMemory{Path: path})
	t.Cleanup(func() { model.SetWindowMemory(nil) })
	a, err := model.New(model.Spec{Type: "openai-compatible", Model: "claude-sonnet-5-5",
		BaseURL: "http://user:sk-pass-secret@gateway.example/v1?api-key=sk-query-secret", APIKey: "sk-header-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if !model.LearnWindow(a, 200_000, true) {
		t.Fatal("not learned")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"user", "sk-pass-secret", "sk-query-secret", "sk-header-secret", "gateway", "api-key", "claude"} {
		if strings.Contains(string(data), leak) {
			t.Fatalf("the file holds %q:\n%s", leak, data)
		}
	}
	if !strings.Contains(string(data), "200000") {
		t.Fatalf("the limit was not kept:\n%s", data)
	}
}
