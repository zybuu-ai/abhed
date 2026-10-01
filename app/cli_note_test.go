package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// noteRig is bangRig with the write tool, as the CLI's registry has it.
func noteRig(t *testing.T, answers ...string) (*cliState, *agent.MemStore, *scriptSurface) {
	t.Helper()
	st, store, sf := bangRig(t, answers...)
	st.loop.Tools = tools.NewRegistry(tools.Bash{}, tools.Write{}, tools.Read{})
	t.Setenv("HOME", t.TempDir())
	return st, store, sf
}

func TestNoteGoesToTheChosenMemory(t *testing.T) {
	for _, tc := range []struct{ answer, file string }{
		{"project", "ABHED.md"}, {"local", "ABHED.local.md"}, {"user", ""},
	} {
		st, store, sf := noteRig(t, tc.answer)
		path := filepath.Join(st.sess.Root, tc.file)
		if tc.file == "" {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, ".abhed", "ABHED.md")
		} else {
			write(t, path, "# Rules\nexisting") // no trailing newline
		}
		addNote(context.Background(), st, nil, "run go vet first")
		data, err := os.ReadFile(path)
		if err != nil || !strings.HasSuffix(string(data), "- run go vet first\n") {
			t.Fatalf("%s: %q %v", tc.answer, data, err)
		}
		if tc.file != "" && !strings.HasPrefix(string(data), "# Rules\nexisting\n") {
			t.Fatalf("%s: the file's content was not kept: %q", tc.answer, data)
		}
		w := storeEventsOf(t, store, agent.EvMemoryWritten)
		if len(w) != 1 || w[0].Actor != agent.ActorUser {
			t.Fatalf("%s: memory.written %+v", tc.answer, w)
		}
		p := payloadOf(t, w[0])
		if p["kind"] != "note" || p["by"] != "user" || (tc.file != "" && p["path"] != tc.file) {
			t.Fatalf("%s: memory.written payload %v", tc.answer, p)
		}
		if len(sf.asked) != 1 {
			t.Fatalf("%s: asked %d times", tc.answer, len(sf.asked))
		}
	}
}

// With no answer nothing is written; a note is never saved by default.
func TestNoteWithoutAnAnswerIsNotSaved(t *testing.T) {
	st, store, sf := noteRig(t, "")
	addNote(context.Background(), st, nil, "remember me")
	if _, err := os.Stat(filepath.Join(st.sess.Root, "ABHED.md")); err == nil {
		t.Fatal("saved without an answer")
	}
	if len(storeEventsOf(t, store, agent.EvMemoryWritten)) != 0 || !strings.Contains(sf.shown(), "not saved") {
		t.Fatalf("shown %q", sf.shown())
	}
}

// A write rule holds for a note, and a memory file that is a link out of
// the workspace is not written through.
func TestNoteFollowsPolicyAndBoundary(t *testing.T) {
	st, store, sf := noteRig(t, "project")
	if err := st.loop.Policy.AddDeny("write(ABHED.md)"); err != nil {
		t.Fatal(err)
	}
	addNote(context.Background(), st, nil, "blocked")
	if _, err := os.Stat(filepath.Join(st.sess.Root, "ABHED.md")); err == nil {
		t.Fatal("a denied write happened")
	}
	if len(storeEventsOf(t, store, agent.EvActionDenied)) != 1 || !strings.Contains(sf.shown(), "write(ABHED.md)") {
		t.Fatalf("denial not recorded and shown: %q", sf.shown())
	}

	st, _, _ = noteRig(t, "local")
	outside := filepath.Join(t.TempDir(), "elsewhere.md")
	write(t, outside, "")
	if err := os.Symlink(outside, filepath.Join(st.sess.Root, "ABHED.local.md")); err != nil {
		t.Skip(err)
	}
	addNote(context.Background(), st, nil, "escaped")
	if data, _ := os.ReadFile(outside); len(data) != 0 {
		t.Fatal("a note was written through a link out of the workspace")
	}
}

// A secret in a note is redacted before it is written.
func TestNoteIsRedacted(t *testing.T) {
	st, _, _ := noteRig(t, "project")
	st.loop.Recorder.Redact = canaryRedactor{}
	addNote(context.Background(), st, nil, "the key is SECRET-CANARY")
	data, _ := os.ReadFile(filepath.Join(st.sess.Root, "ABHED.md"))
	if strings.Contains(string(data), "SECRET-CANARY") || !strings.Contains(string(data), "[redacted]") {
		t.Fatalf("written: %q", data)
	}
}

// A note keeps ABHED.md's mode: a shared file stays readable.
func TestNoteKeepsTheFileMode(t *testing.T) {
	st, _, _ := noteRig(t, "project")
	p := filepath.Join(st.sess.Root, "ABHED.md")
	write(t, p, "x\n")
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	addNote(context.Background(), st, nil, "y")
	if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v %v", info.Mode(), err)
	}
}
