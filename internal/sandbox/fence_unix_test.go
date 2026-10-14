//go:build unix

package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// An entry that can be neither quarantined nor removed is reported as still
// present, and the next fence refuses the .abhed rather than take it for state.
func TestFenceStateMountReportsWhatCannotBeRemoved(t *testing.T) {
	permsMatter(t)
	p := fencePolicy(t)
	state := filepath.Join(p.Workspace, ".abhed")
	f := NewFence(p)
	f.mounts = true
	f.id = "1-test"
	if err := f.prepareStateMount(); err != nil {
		t.Fatal(err)
	}
	// An entry that cannot be removed: no mode stops the owner's walk now.
	locked := filepath.Join(state, "planted")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	old := removeTakenOut
	removeTakenOut = func(string) error { return os.ErrPermission }
	t.Cleanup(func() { removeTakenOut = old })
	home, _ := os.UserHomeDir()
	// The fence's lock folder is there already; the file takes its place.
	_ = os.RemoveAll(filepath.Join(home, ".abhed"))
	if err := os.WriteFile(filepath.Join(home, ".abhed"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	err := f.checkPlanted(func(_ string, pay map[string]any) error { got = append(got, pay); return nil }, "", "after_command")
	if err == nil || !strings.Contains(err.Error(), "still in the workspace") {
		t.Fatalf("not reported as left: %v", err)
	}
	contents, _ := got[0]["entries"].([]map[string]any)[0]["contents"].([]map[string]any)
	if len(contents) != 1 || contents[0]["outcome"] != plantRemaining || got[0]["still_present"] != true {
		t.Fatalf("event: %v", got[0])
	}
	renamed, _ := contents[0]["renamed_to"].(string)
	if filepath.Dir(renamed) != state {
		t.Fatalf("renamed to %q, outside the covered .abhed", renamed)
	}
	g := NewFence(p)
	g.mounts = true
	if err := g.prepareStateMount(); err == nil || !strings.Contains(err.Error(), "could not take out") || g.stateHeld {
		t.Fatalf("the next fence: %v held %v", err, g.stateHeld)
	}
}

// An empty .abhed that was there before the fence is left at Close; only one
// a fence made is removed.
func TestFenceLeavesAnEmptyStateItDidNotMake(t *testing.T) {
	p := fencePolicy(t)
	state := filepath.Join(p.Workspace, ".abhed")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	f := NewFence(p)
	f.mounts = true
	if err := f.prepareStateMount(); err != nil || f.madeState || f.stateHeld {
		t.Fatalf("prepare: %v made %v held %v", err, f.madeState, f.stateHeld)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(state); err != nil || !info.IsDir() {
		t.Fatalf("an empty .abhed the fence did not make was removed: %v", err)
	}
}

// A planted tree locked deeper than any bound is still taken out whole and
// left readable and removable by its owner; a file in it hard-linked to one
// outside keeps that file's mode, while the tree's own locked file is given back.
func TestFenceTakesOutADeepLockedTree(t *testing.T) {
	permsMatter(t)
	p := fencePolicy(t)
	f := NewFence(p)
	f.id = "1-test"
	outside := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(outside, []byte("k"), 0o400); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(p.Workspace, ".abhed")
	deep := filepath.Join(dir, strings.Repeat("d"+string(filepath.Separator), 300))
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	// Off Linux, where no command is fenced, a file is reopened to change it,
	// so one its owner can neither read nor write is left as it is.
	lockedFile := os.FileMode(0o000)
	if runtime.GOOS != "linux" {
		lockedFile = 0o200
	}
	if err := os.WriteFile(filepath.Join(deep, "own"), []byte("o"), lockedFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(deep, "linked")); err != nil {
		t.Fatal(err)
	}
	for d := deep; d != filepath.Dir(dir); d = filepath.Dir(d) {
		if err := os.Chmod(d, 0o100); err != nil {
			t.Fatal(err)
		}
	}
	var got []map[string]any
	if err := f.checkPlanted(func(_ string, pay map[string]any) error { got = append(got, pay); return nil }, "", "after_command"); err == nil {
		t.Fatal("not reported")
	}
	entries, _ := got[0]["entries"].([]map[string]any)
	if len(entries) != 1 || entries[0]["outcome"] != plantMoved {
		t.Fatalf("entries %v", entries)
	}
	dest := entries[0]["moved_to"].(string)
	inner := filepath.Join(dest, strings.Repeat("d"+string(filepath.Separator), 300))
	if b, err := os.ReadFile(filepath.Join(inner, "own")); err != nil || string(b) != "o" {
		t.Fatalf("the locked file 300 folders down was not given back: %v", err)
	}
	if fi, err := os.Stat(outside); err != nil || fi.Mode().Perm() != 0o400 {
		t.Fatalf("the file outside, hard-linked in, became %v (%v)", fi.Mode(), err)
	}
	if err := os.RemoveAll(filepath.Dir(dest)); err != nil {
		t.Fatalf("the quarantined tree cannot be removed: %v", err)
	}
}
