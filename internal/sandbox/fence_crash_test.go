//go:build unix

package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A session that ended without its last check, with no .abhed when it
// started, has the .abhed dating from it taken out at the next start; one
// made after its last command, or a workspace that had one, is left.
func TestFenceSweepsWhatACrashedSessionLeft(t *testing.T) {
	p := fencePolicy(t)
	f := NewFence(p)
	f.id = "next"
	dir, err := markDir()
	if err != nil {
		t.Fatal(err)
	}
	mark := func(name string, state bool, at time.Time) {
		t.Helper()
		b, _ := json.Marshal(fenceMark{PID: deadPID(t), State: state})
		path := filepath.Join(dir, markPrefix(p.Workspace)+name+".mark")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(p.Workspace, ".abhed")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(state, old, old); err != nil {
		t.Fatal(err)
	}
	// Made after the crashed session's last command: left.
	mark("a", false, old.Add(-time.Hour))
	var got []map[string]any
	rec := func(_ string, pay map[string]any) error { got = append(got, pay); return nil }
	f.sweepCrashed(rec)
	if _, err := os.Lstat(state); err != nil || len(got) != 0 {
		t.Fatalf("a later .abhed was taken: %v %v", err, got)
	}
	// From the crashed session: taken out, and its mark removed.
	mark("b", false, time.Now())
	f.sweepCrashed(rec)
	if _, err := os.Lstat(state); err == nil || len(got) != 1 {
		t.Fatalf("the crashed session's .abhed was left: %v", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, markPrefix(p.Workspace)+"*.mark")); len(left) != 0 {
		t.Fatalf("marks left: %v", left)
	}
}

// deadPID is a process id no process has.
func deadPID(t *testing.T) int {
	for pid := 4_000_000; pid > 3_000_000; pid -= 7919 {
		if !processAlive(pid) {
			return pid
		}
	}
	t.Skip("no free process id found")
	return 0
}
