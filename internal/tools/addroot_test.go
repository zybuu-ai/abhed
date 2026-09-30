package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every way a root is added, the -add-dir flag and config included, refuses
// credential folders, a folder holding the home directory, and one holding
// Abhed's state; a folder inside ~/.abhed that holds none, such as skills,
// is allowed. A path that no longer leads where it was checked is refused.
func TestAddRootRefusesStateAndCredentials(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for _, d := range []string{".ssh/keys", ".aws", ".abhed/skills/x", "proj", "other"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "secrets.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".ssh", ".ssh/keys", ".aws", ".abhed", "", ".."} {
		if err := s.AddRoot(filepath.Join(home, d)); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("%s: %v", d, err)
		}
	}
	if err := s.AddRoot(filepath.Dir(home)); err == nil || !strings.Contains(err.Error(), "holds your home directory") {
		t.Errorf("the folder holding home: %v", err)
	}
	if err := s.AddRoot(filepath.Join(home, ".abhed", "skills", "x")); err != nil {
		t.Errorf("a skill folder was refused: %v", err)
	}
	if got, err := s.AddRootAs(filepath.Join(home, "proj"), filepath.Join(home, "other")); err == nil || got != "" {
		t.Fatalf("a path leading elsewhere than checked was added: %q %v", got, err)
	}
	if got, err := s.AddRootAs(filepath.Join(home, "proj"), filepath.Join(home, "proj")); err != nil || got != filepath.Join(home, "proj") {
		t.Fatalf("%q %v", got, err)
	}
}

// A folder that holds the workspace, as a monorepo's root does, may be added,
// whether or not the workspace has a .abhed of its own.
func TestAddRootAllowsAFolderHoldingTheWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mono, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(mono, "pkg")
	if err := os.MkdirAll(filepath.Join(pkg, StateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, StateDir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddRoot(mono); err != nil {
		t.Fatalf("the monorepo root was refused: %v", err)
	}
	if _, err := s.Resolve(filepath.Join(pkg, StateDir, "config.json")); err == nil {
		t.Fatal("the workspace's .abhed became reachable through the added root")
	}
}
