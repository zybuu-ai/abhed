//go:build unix

package sandbox

import (
	"os"
	"path/filepath"
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
	// Deeper than restoreOwnerAccess reaches, a folder without write
	// permission keeps RemoveAll from emptying it.
	deep := strings.Repeat("d"+string(filepath.Separator), ownerAccessDepth+2)
	locked := filepath.Join(state, "planted", deep)
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
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
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(renamed, deep), 0o700) })
	if filepath.Dir(renamed) != state {
		t.Fatalf("renamed to %q, outside the covered .abhed", renamed)
	}
	g := NewFence(p)
	g.mounts = true
	if err := g.prepareStateMount(); err == nil || !strings.Contains(err.Error(), "could not take out") || g.stateHeld {
		t.Fatalf("the next fence: %v held %v", err, g.stateHeld)
	}
}
