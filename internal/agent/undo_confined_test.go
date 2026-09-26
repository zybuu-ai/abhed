package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// Undo restores and removes by the session's rules: a folder swapped for a
// link, to outside the workspace or into Abhed's state, cannot turn a restore
// into an overwrite there, nor an undone creation into a deletion there.
func TestUndoStaysInTheWorkspaceThroughSwappedLinks(t *testing.T) {
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	outside := t.TempDir()
	s, err := tools.NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(ws, tools.StateDir)
	for _, dir := range []string{state, outside, filepath.Join(ws, "d-dir")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The same names in the decoy folder, outside and in the state.
	for _, dir := range []string{filepath.Join(ws, "d-dir"), outside, state} {
		for _, name := range []string{"users.json", "made.txt"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	decoyDir, decoy := filepath.Join(ws, "d-dir"), filepath.Join(ws, "decoy")
	links := []string{filepath.Join(ws, "to-outside"), filepath.Join(ws, "to-state")}
	if err := os.Symlink(outside, links[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(state, links[1]); err != nil {
		t.Fatal(err)
	}
	u := NewUndoLog(s.RestoreFile, s.RemoveFile)

	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, from := range append([]string{decoyDir}, links...) {
				_ = os.Rename(from, decoy)
				_ = os.Rename(decoy, from)
			}
		}
	}()
	for i := 0; i < 2000; i++ {
		u.BeginTurn()
		u.Record(filepath.Join(decoy, "users.json"), []byte("overwritten"), true)
		u.Record(filepath.Join(decoy, "made.txt"), nil, false)
		_, _ = u.Undo()
	}
	close(stop)
	<-done
	for _, dir := range []string{outside, state} {
		for _, name := range []string{"users.json", "made.txt"} {
			got, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || string(got) != "keep" {
				t.Errorf("undo reached %s: %q, %v", filepath.Join(dir, name), got, err)
			}
		}
	}
}
