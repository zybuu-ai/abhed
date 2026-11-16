package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// editingSurface is a scripted surface whose editor replaces what it is
// given with what the test chose, and remembers what it was shown.
type editingSurface struct {
	*scriptSurface
	result string
	shown  string
}

func (s *editingSurface) EditText(_ context.Context, text string) (string, error) {
	s.shown = text
	return s.result, nil
}

func TestMemoryEditOpensAndSavesTheFile(t *testing.T) {
	for _, tc := range []struct{ target, file string }{
		{"project", "ABHED.md"}, {"local", "ABHED.local.md"}, {"user", ""},
	} {
		st, store, sf := noteRig(t)
		path := filepath.Join(st.sess.Root, tc.file)
		if tc.file == "" {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, ".abhed", "ABHED.md")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		write(t, path, "# Rules\nold\n")
		ed := &editingSurface{scriptSurface: sf, result: "# Rules\nnew\n"}
		e := &cmdEnv{ui: ed, st: st, pol: st.loop.Policy, sess: st.sess}
		if _, err := slashMemory(context.Background(), e, []string{"edit", tc.target}); err != nil {
			t.Fatalf("%s: %v", tc.target, err)
		}
		if ed.shown != "# Rules\nold\n" {
			t.Fatalf("%s: the editor was given %q", tc.target, ed.shown)
		}
		if data, _ := os.ReadFile(path); string(data) != "# Rules\nnew\n" {
			t.Fatalf("%s: saved %q", tc.target, data)
		}
		w := storeEventsOf(t, store, agent.EvMemoryWritten)
		if len(w) != 1 || w[0].Actor != agent.ActorUser || payloadOf(t, w[0])["kind"] != "edit" {
			t.Fatalf("%s: memory.written %+v", tc.target, w)
		}
	}
}

// An edit is the person's write: a write rule still holds, and nothing
// unchanged is written or recorded.
func TestMemoryEditFollowsPolicy(t *testing.T) {
	st, _, sf := noteRig(t)
	write(t, filepath.Join(st.sess.Root, "ABHED.md"), "keep\n")
	if err := st.loop.Policy.AddDeny("write(ABHED.md)"); err != nil {
		t.Fatal(err)
	}
	e := &cmdEnv{ui: &editingSurface{scriptSurface: sf, result: "changed\n"}, st: st, pol: st.loop.Policy, sess: st.sess}
	if _, err := slashMemory(context.Background(), e, []string{"edit", "project"}); err == nil || !strings.Contains(err.Error(), "write(ABHED.md)") {
		t.Fatalf("a denied edit: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(st.sess.Root, "ABHED.md")); string(data) != "keep\n" {
		t.Fatalf("a denied edit wrote %q", data)
	}

	st, store, sf := noteRig(t)
	write(t, filepath.Join(st.sess.Root, "ABHED.md"), "same\n")
	e = &cmdEnv{ui: &editingSurface{scriptSurface: sf, result: "same\n"}, st: st, pol: st.loop.Policy, sess: st.sess}
	if _, err := slashMemory(context.Background(), e, []string{"edit", "project"}); err != nil {
		t.Fatal(err)
	}
	if len(storeEventsOf(t, store, agent.EvMemoryWritten)) != 0 || !strings.Contains(sf.shown(), "unchanged") {
		t.Fatalf("an unchanged edit: %q", sf.shown())
	}

	// Without a terminal there is no editor to open.
	st, _, sf = noteRig(t)
	e = &cmdEnv{ui: sf, st: st, pol: st.loop.Policy, sess: st.sess}
	if _, err := slashMemory(context.Background(), e, []string{"edit", "project"}); err == nil {
		t.Fatal("an edit without an editor succeeded")
	}
}
