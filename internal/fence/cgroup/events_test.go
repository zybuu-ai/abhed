package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

// A refused fork is counted whether the kernel puts it in the forking
// task's leaf (before Linux 6.14) or in the cgroup whose limit it hit.
func TestEventsCountRefusedForksBelow(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, s string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pids.events", "max 0\n")
	write("leaf-a/pids.events", "max 4722\n")
	write("leaf-b/pids.events", "max 3\n")
	ev, err := node{dir}.Events()
	if err != nil || ev.PidsMax != 4725 {
		t.Fatalf("old kernel: %d %v", ev.PidsMax, err)
	}
	write("pids.events", "max 5000\n")
	if ev, _ := (node{dir}).Events(); ev.PidsMax != 5000 {
		t.Fatalf("hierarchical counting: %d", ev.PidsMax)
	}
}

// OwnPidsMax reads the cgroup's own count alone.
func TestOwnPidsMaxIgnoresChildren(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "leaf"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "pids.events"), []byte("max 2\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "leaf", "pids.events"), []byte("max 9\n"), 0o644)
	if got := (node{dir}).OwnPidsMax(); got != 2 {
		t.Fatalf("own %d", got)
	}
}
