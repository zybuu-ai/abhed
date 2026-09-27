package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeWorkspace is a session on a workspace holding Abhed's state, links to it
// by absolute and relative path, and a link to a folder outside.
func writeWorkspace(t *testing.T) (s *Session, ws, outsideDir string) {
	t.Helper()
	ws = t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	outsideDir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, StateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, StateDir, "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, StateDir), filepath.Join(ws, "settings")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if err := os.Symlink(StateDir, filepath.Join(ws, "statelink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(ws, "elsewhere")); err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	return s, ws, outsideDir
}

// A new file in folders that do not exist yet is written, folders and all:
// accept-edits and auto no longer need a shell approval for a new folder.
func TestWriteCreatesMissingFolders(t *testing.T) {
	s, ws, _ := writeWorkspace(t)
	path := filepath.Join(ws, "services", "billing", "ledger", "rates.py")
	raw, _ := json.Marshal(map[string]string{"path": path, "content": "RATE = 3\n"})
	if res := (Write{}).Run(context.Background(), s, raw); res.IsError {
		t.Fatalf("write refused: %s", res.Content)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "RATE = 3\n" {
		t.Fatalf("file %q, %v", got, err)
	}
}

// The folders a write makes are never state and never outside the
// workspace, however the path reaches there.
func TestWriteMakesNoFolderInStateOrOutside(t *testing.T) {
	s, ws, outsideDir := writeWorkspace(t)
	for _, path := range []string{
		filepath.Join(ws, StateDir, "made", "x.txt"),
		filepath.Join(ws, "settings", "made", "x.txt"),
		filepath.Join(ws, "statelink", "made", "x.txt"),
		filepath.Join(ws, "elsewhere", "made", "x.txt"),
		filepath.Join(ws, "..", filepath.Base(outsideDir), "made", "x.txt"),
	} {
		raw, _ := json.Marshal(map[string]string{"path": path, "content": "x"})
		if res := (Write{}).Run(context.Background(), s, raw); !res.IsError {
			t.Errorf("%s: written: %s", path, res.Content)
		}
	}
	for _, dir := range []string{filepath.Join(ws, StateDir, "made"), filepath.Join(outsideDir, "made")} {
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("%s was created", dir)
		}
	}
}

// Below the path checks, the confined root makes each folder in its parent
// and refuses one that is, or leads into, state or out of the workspace.
func TestConfinedMkdirAllKeepsToTheRoot(t *testing.T) {
	s, ws, outsideDir := writeWorkspace(t)
	c, err := s.stateSet().Confine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.MkdirAll(filepath.Join(ws, "a", "b", "c"), 0o755); err != nil {
		t.Fatalf("plain folders: %v", err)
	}
	if err := c.MkdirAll(filepath.Join(ws, "settings", "made", "deeper"), 0o755); !errors.Is(err, ErrState) {
		t.Errorf("through a link to state: %v, want ErrState", err)
	}
	if err := c.MkdirAll(filepath.Join(ws, "statelink", "made"), 0o755); !errors.Is(err, ErrState) {
		t.Errorf("through a relative link to state: %v, want ErrState", err)
	}
	if err := c.MkdirAll(filepath.Join(ws, "elsewhere", "made"), 0o755); !errors.Is(err, ErrOutside) {
		t.Errorf("through a link out: %v, want ErrOutside", err)
	}
	for _, dir := range []string{filepath.Join(ws, StateDir, "made"), filepath.Join(outsideDir, "made")} {
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("%s was created", dir)
		}
	}
}

// A folder swapped for a link into state just after it was made is caught by
// the identity check on the last folder, which no path check sees.
func TestConfinedMkdirAllJudgesTheLastFolderMade(t *testing.T) {
	_, ws, _ := writeWorkspace(t)
	s, err := NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.stateSet().Confine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	made := filepath.Join(ws, "made")
	afterMkdir = func(at string) {
		if at == "made" {
			_ = os.Remove(made)
			_ = os.Symlink(StateDir, made)
		}
	}
	defer func() { afterMkdir = func(string) {} }()
	if err := c.MkdirAll(made, 0o755); !errors.Is(err, ErrState) {
		t.Fatalf("a last folder swapped into state: %v, want ErrState", err)
	}
}

// On a disk that ignores case, .ABHED is the state folder, and no folder is
// made in it by that spelling.
func TestConfinedMkdirAllRefusesStateInAnyCase(t *testing.T) {
	s, ws, _ := writeWorkspace(t)
	c, err := s.stateSet().Confine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, name := range []string{".ABHED", ".Abhed"} {
		if err := c.MkdirAll(filepath.Join(ws, name, "made"), 0o755); !errors.Is(err, ErrState) {
			t.Errorf("%s/made: %v, want ErrState", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, StateDir, "made")); err == nil {
		t.Error("a folder was made in state")
	}
	for _, name := range []string{".ABHED", ".Abhed"} {
		if _, err := os.Lstat(filepath.Join(ws, name)); err == nil && !sameFolder(t, filepath.Join(ws, name), filepath.Join(ws, StateDir)) {
			t.Errorf("a folder %s was made beside the state folder", name)
		}
	}
}

func sameFolder(t *testing.T, a, b string) bool {
	t.Helper()
	ia, err1 := os.Stat(a)
	ib, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(ia, ib)
}

// Undoing a new file in new folders removes the folders the write made, and
// only those; a failed write leaves none behind.
func TestWriteFoldersGoWithTheirFile(t *testing.T) {
	s, ws, _ := writeWorkspace(t)
	if err := os.MkdirAll(filepath.Join(ws, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ws, "keep", "new", "deeper", "a.txt")
	raw, _ := json.Marshal(map[string]string{"path": path, "content": "a"})
	if res := (Write{}).Run(context.Background(), s, raw); res.IsError {
		t.Fatal(res.Content)
	}
	if err := s.RemoveFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "keep", "new")); err == nil {
		t.Error("the folders the write made outlived its undo")
	}
	if _, err := os.Stat(filepath.Join(ws, "keep")); err != nil {
		t.Error("undo removed a folder the write did not make")
	}

	// A write that fails after making its folders takes them back.
	afterMkdir = func(at string) {
		if at == filepath.Join("fresh", "sub") {
			_ = os.Chmod(filepath.Join(ws, at), 0o555) // the file cannot be created in it
		}
	}
	defer func() { afterMkdir = func(string) {} }()
	raw, _ = json.Marshal(map[string]string{"path": filepath.Join(ws, "fresh", "sub", "x.txt"), "content": "x"})
	if res := (Write{}).Run(context.Background(), s, raw); !res.IsError {
		t.Skipf("a write into a read-only folder succeeded here: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(ws, "fresh")); err == nil {
		t.Error("a failed write left the folders it made")
	}
}
