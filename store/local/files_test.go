package local

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
)

// The review's probe: files found wider than 0600 are made private when the
// record opens them.
func TestWideFilesAreMadePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes do not apply on Windows")
	}
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-1", "one")
	_ = s.Close()
	for _, p := range []string{filepath.Join(dir, "default", "s-1.jsonl"), filepath.Join(dir, "default", "index.jsonl"), filepath.Join(dir, "default", "head", "s-1")} {
		_ = os.Chmod(p, 0o644)
	}
	s2 := openTest(t, dir)
	rec := agent.NewRecorder(s2, "s-1", "")
	rec.Advance(1)
	if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "two"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s2.Path("s-1"), s2.index.path(), s2.headPath("s-1")} {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Errorf("%s left at %o", filepath.Base(p), st.Mode().Perm())
		}
	}
}

// The review's probe: a session file that is a link to elsewhere is not
// written through; nor is one with a second name.
func TestLinkedSessionFilesAreRefused(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-0", "x")
	outside := filepath.Join(t.TempDir(), "planted.jsonl")
	_ = os.WriteFile(outside, nil, 0o644)
	_ = os.Symlink(outside, s.Path("s-9"))
	_ = s.CreateSession(t.Context(), store.SessionRecord{ID: "s-9"})
	rec := agent.NewRecorder(s, "s-9", "")
	if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "hi"}); err == nil {
		t.Fatal("an append through a link was taken")
	}
	if data, _ := os.ReadFile(outside); len(data) != 0 {
		t.Fatalf("written through the link: %d bytes", len(data))
	}
	_ = s.Release("s-0")
	if err := os.Link(s.Path("s-0"), filepath.Join(t.TempDir(), "second")); err != nil {
		t.Skip("no hard links here")
	}
	if err := s.Acquire("s-0"); err == nil {
		t.Fatal("a session file with a second name was opened for writing")
	}
}
