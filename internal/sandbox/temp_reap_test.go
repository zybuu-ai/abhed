package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The caches and temp folders an ended Abhed left are removed, by their
// owner file; a live owner's, one without an owner file and another name
// are kept, and a read-only tree inside is no obstacle.
func TestReapStaleTemps(t *testing.T) {
	parent := t.TempDir()
	live := ownerOf(os.Getpid())
	if live == "" {
		t.Skip("this platform cannot read a process's start")
	}
	mk := func(name, owner string) string {
		d := filepath.Join(parent, name)
		if err := os.MkdirAll(filepath.Join(d, "go-mod", "x"), 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(d, "go-mod", "x", "f"), []byte("x"), 0o444)
		_ = os.Chmod(filepath.Join(d, "go-mod", "x"), 0o555)
		if owner != "" {
			_ = os.WriteFile(filepath.Join(d, tempOwnerFile), []byte(owner), 0o400)
		}
		return d
	}
	dead := hostID() + ":424242:1"
	old := ownerExists
	t.Cleanup(func() { ownerExists = old })
	ownerExists = func(pid int) existence {
		if pid == 424242 {
			return existsNo
		}
		return old(pid)
	}
	gone := []string{mk("abhed-caches-dead1", dead), mk("abhed-tmp-dead2", dead)}
	kept := []string{mk("abhed-caches-live", live), mk("abhed-caches-nomark", ""), mk("other-caches-dead", dead), mk("abhed-caches-", dead)}
	t.Cleanup(func() {
		for _, d := range kept {
			makeRemovable(d)
		}
	})
	reapTemps([]string{parent})
	for _, d := range gone {
		if _, err := os.Stat(d); err == nil {
			t.Errorf("%s was kept", d)
		}
	}
	for _, d := range kept {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("%s was removed", d)
		}
	}
}

// A sandbox's own caches carry this process as owner and go on Close.
func TestOwnCachesAreMarkedAndRemoved(t *testing.T) {
	s := NewProcess(DefaultPolicy(workspace(t)))
	if s.backend != "sandbox-exec" {
		t.Skip("own caches are the macOS process tier's")
	}
	own := s.ownCaches()
	if len(own) == 0 {
		t.Fatal("no caches")
	}
	if ownerOfTemp(s.caches) != ownerLives {
		t.Fatalf("the caches do not name this process: %s", s.caches)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.caches); err == nil {
		t.Fatal("the caches outlived Close")
	}
}

// A debug session's socket folder is made in the home's .abhed, which every
// command's profile denies, owned for the reaper, which removes one an
// ended Abhed left.
func TestDebugSocketDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := DebugSocketDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != filepath.Join(home, ".abhed", "run") || !strings.HasPrefix(filepath.Base(dir), "abhed-dap-") {
		t.Fatalf("dir %s", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, tempOwnerFile)); err != nil {
		t.Fatalf("no owner file: %v", err)
	}
	if !hasReapedPrefix(filepath.Base(dir)) || !slices.Contains(reapParents(), filepath.Join(home, ".abhed", "run")) {
		t.Fatal("not reaped")
	}
	prof := NewProcess(DefaultPolicy(t.TempDir())).seatbeltProfileFor(0)
	if !strings.Contains(prof, fmt.Sprintf("(deny file-read* (subpath %q))", filepath.Join(home, ".abhed"))) {
		t.Fatal("the folder's parent is not denied to commands")
	}
}
