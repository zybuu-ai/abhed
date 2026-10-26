package lsprelay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func touch(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The check never takes "could not look" for "none": a walk that stops at a
// bound says so, a link with the name counts, and the root itself is
// looked at before the walk.
func TestRustAnalyzerConfigsFailTowardAsking(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "src", "main.rs"), "")
	if files, incomplete := RustAnalyzerConfigs([]string{root}); len(files) != 0 || incomplete != "" {
		t.Fatalf("a clean tree: %v %q", files, incomplete)
	}

	// A link named rust-analyzer.toml, at the root and deep in the tree.
	touch(t, filepath.Join(root, "notes.txt"), "[cargo]\n")
	if err := os.Symlink("notes.txt", filepath.Join(root, "rust-analyzer.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "crates", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../notes.txt", filepath.Join(root, "crates", "a", ".rust-analyzer.toml")); err != nil {
		t.Fatal(err)
	}
	files, _ := RustAnalyzerConfigs([]string{root})
	if len(files) != 2 {
		t.Fatalf("links named rust-analyzer.toml: %v", files)
	}

	// Many entries ahead of the root's file: the walk stops and says so,
	// and the root's own file is still found.
	old := raMaxEntries
	raMaxEntries = 20
	t.Cleanup(func() { raMaxEntries = old })
	for i := range 30 {
		touch(t, filepath.Join(root, "aaa", strings.Repeat("f", i+1)), "")
	}
	files, incomplete := RustAnalyzerConfigs([]string{root})
	if incomplete == "" || len(files) == 0 || files[0] != filepath.Join(root, "rust-analyzer.toml") {
		t.Fatalf("past the entry bound: %v %q", files, incomplete)
	}
	if why := RustAnalyzerConfigWhy(nil, incomplete); !strings.Contains(why, "could not check every folder") {
		t.Errorf("why: %q", why)
	}
	raMaxEntries = old

	// Too deep: said, not skipped silently.
	deep := t.TempDir()
	touch(t, filepath.Join(deep, filepath.Join(strings.Split(strings.Repeat("d/", raMaxDepth+1), "/")...), "x.rs"), "")
	if _, incomplete := RustAnalyzerConfigs([]string{deep}); !strings.Contains(incomplete, "levels deep") {
		t.Fatalf("past the depth bound: %q", incomplete)
	}
	// target, node_modules and .git are left out, without saying so.
	skip := t.TempDir()
	touch(t, filepath.Join(skip, "target", "rust-analyzer.toml"), "")
	if files, incomplete := RustAnalyzerConfigs([]string{skip}); len(files) != 0 || incomplete != "" {
		t.Fatalf("skipped folders: %v %q", files, incomplete)
	}
}

// The watch sees a rust-analyzer.toml appear in a root or a parent, a found
// one change, and one go.
func TestRustAnalyzerWatch(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ws")
	deep := filepath.Join(root, "crates", "a", "rust-analyzer.toml")
	touch(t, deep, "a")
	w := NewRustAnalyzerWatch([]string{root}, []string{deep})
	if w.Changed() {
		t.Fatal("changed with nothing done")
	}
	touch(t, filepath.Join(parent, ".rust-analyzer.toml"), "")
	if !w.Changed() {
		t.Fatal("a new file in a parent was not seen")
	}
	w = NewRustAnalyzerWatch([]string{root}, []string{deep})
	touch(t, deep, "changed")
	_ = os.Chtimes(deep, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	if !w.Changed() {
		t.Fatal("a change to a found file was not seen")
	}
	w = NewRustAnalyzerWatch([]string{root}, []string{deep})
	_ = os.Remove(deep)
	if !w.Changed() {
		t.Fatal("a removed file was not seen")
	}
}

func TestRustAnalyzerConfigCache(t *testing.T) {
	root := t.TempDir()
	var c RustAnalyzerConfigCache
	if files, _ := c.Get([]string{root}, "1"); len(files) != 0 {
		t.Fatal(files)
	}
	touch(t, filepath.Join(root, "rust-analyzer.toml"), "")
	if files, _ := c.Get([]string{root}, "1"); len(files) != 0 {
		t.Fatal("the cache was not used")
	}
	if files, _ := c.Get([]string{root}, "2"); len(files) != 1 {
		t.Fatal("a new stamp did not walk again")
	}
	_ = os.Remove(filepath.Join(root, "rust-analyzer.toml"))
	c.Invalidate()
	if files, _ := c.Get([]string{root}, "2"); len(files) != 0 {
		t.Fatal("Invalidate did not walk again")
	}
}

func TestNamesRustAnalyzerConfig(t *testing.T) {
	for body, want := range map[string]bool{
		`{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file:///w/crates/a/rust-analyzer.toml"}}}`:                                  true,
		`{"jsonrpc":"2.0","method":"workspace/didChangeWatchedFiles","params":{"changes":[{"uri":"file:///w/.rust-analyzer.toml","type":1}]}}`:                         true,
		`{"jsonrpc":"2.0","method":"workspace/didRenameFiles","params":{"files":[{"oldUri":"file:///w/x","newUri":"file:///w/rust-analyzer.toml"}]}}`:                  true,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"file:///w/README.md"},"contentChanges":[{"text":"rust-analyzer.toml"}]}}`: false,
		`{"jsonrpc":"2.0","id":1,"method":"textDocument/hover","params":{"textDocument":{"uri":"file:///w/rust-analyzer.toml"}}}`:                                      false,
	} {
		if got := NamesRustAnalyzerConfig([]byte(body)); got != want {
			t.Errorf("%s: %v", body, got)
		}
	}
}
