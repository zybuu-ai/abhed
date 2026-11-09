package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

func subdirFixture(t *testing.T, dir string) {
	t.Helper()
	for name, body := range map[string]string{
		"svc/ABHED.md":      "svc rule",
		"svc/a.go":          "package a",
		"svc/b.go":          "package b",
		"svc/deep/ABHED.md": "deep rule",
		"svc/deep/c.go":     "package c",
		".git/ABHED.md":     "git rule",
		".git/config":       "[core]",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A folder's ABHED.md comes with the first result that used the folder, once,
// and is recorded; git's and Abhed's own folders are never memory.
func TestSubdirMemoryLoadedOnceWhenUsed(t *testing.T) {
	dir := tempDir(t)
	subdirFixture(t, dir)
	l, store := harnessIn(t, dir, []scriptedTurn{
		{calls: []model.ToolCall{call("read", map[string]string{"path": "svc/a.go"})}},
		{calls: []model.ToolCall{call("read", map[string]string{"path": "svc/b.go"})}},
		{calls: []model.ToolCall{call("read", map[string]string{"path": "svc/deep/c.go"})}},
		{calls: []model.ToolCall{call("read", map[string]string{"path": ".git/config"})}},
		{text: "done"},
	}, policy.ModeDefault, true)
	l.SubdirMemory = &SubdirMemory{Workspace: dir}
	if _, err := l.Run(context.Background(), "look"); err != nil {
		t.Fatal(err)
	}
	reqs := l.Adapter.(*scriptedAdapter).gotRequests
	last, _ := json.Marshal(reqs[len(reqs)-1].Messages)
	for rule, want := range map[string]int{"svc rule": 1, "deep rule": 1, "git rule": 0} {
		if got := strings.Count(string(last), rule); got != want {
			t.Fatalf("%q appears %d times, want %d", rule, got, want)
		}
	}
	loaded := eventsOf(t, store, EvMemoryLoaded)
	if len(loaded) != 2 {
		t.Fatalf("memory.loaded %d times, want 2: %s", len(loaded), loaded)
	}
	var first MemoryLoaded
	if err := json.Unmarshal(loaded[0], &first); err != nil || len(first.Files) != 1 ||
		first.Files[0].Scope != MemorySubdir || first.Files[0].Path != "svc/ABHED.md" || first.Files[0].SHA256 == "" {
		t.Fatalf("first memory.loaded %s", loaded[0])
	}
}

// A read rule that keeps a folder's file out keeps its memory out too.
func TestSubdirMemoryFollowsReadRules(t *testing.T) {
	dir := tempDir(t)
	subdirFixture(t, dir)
	l, store := harnessIn(t, dir, []scriptedTurn{
		{calls: []model.ToolCall{call("read", map[string]string{"path": "svc/a.go"})}},
		{text: "done"},
	}, policy.ModeDefault, true)
	l.SubdirMemory = &SubdirMemory{Workspace: dir, Allow: func(string) error { return errors.New("denied by read(svc/ABHED.md)") }}
	if _, err := l.Run(context.Background(), "look"); err != nil {
		t.Fatal(err)
	}
	reqs := l.Adapter.(*scriptedAdapter).gotRequests
	last, _ := json.Marshal(reqs[len(reqs)-1].Messages)
	if strings.Contains(string(last), "svc rule") || len(eventsOf(t, store, EvMemoryLoaded)) != 0 {
		t.Fatal("a denied memory file was loaded")
	}
}

// Git's and Abhed's own folders hold no memory, and a path outside the
// workspace loads nothing.
func TestSubdirMemorySkipsStateFolders(t *testing.T) {
	dir := tempDir(t)
	subdirFixture(t, dir)
	m := &SubdirMemory{Workspace: dir}
	if got := m.discover([]string{filepath.Join(dir, ".git", "config"), filepath.Join(filepath.Dir(dir), "x")}); len(got) != 0 {
		t.Fatalf("discovered %+v", got)
	}
}
