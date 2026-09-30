package local

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// The record is Abhed's state: the agent's file tools refuse it by any
// spelling, at ~/.abhed/records and at a managed record.dir.
func TestAgentCannotReachTheRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()
	s, err := Open(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if want, _ := filepath.EvalSymlinks(filepath.Join(home, ".abhed", "records")); s.Dir() != want {
		t.Fatalf("default dir %s, want %s", s.Dir(), want)
	}
	record(t, s, "s-1", "a secret plan")

	managed := filepath.Join(t.TempDir(), "org-records")
	m, err := Open(Options{Dir: managed})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	record(t, m, "s-2", "another")
	tools.AddStatePath(managed)

	link := filepath.Join(ws, "notes")
	if err := os.Symlink(filepath.Join(home, ".abhed", "records", "default"), link); err != nil {
		t.Fatal(err)
	}
	sess, err := tools.NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.Path("s-1"), filepath.Join(link, "s-1.jsonl"), m.Path("s-2"), filepath.Join(managed, "default", "index.jsonl")} {
		if !tools.IsState(p, ws) {
			t.Errorf("%s is not state", p)
		}
		if _, err := sess.ReadFile(p); err == nil {
			t.Errorf("the agent's session read %s", p)
		}
	}
}
