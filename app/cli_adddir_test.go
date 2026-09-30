package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func pathCall(p string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"path": p})
	return b
}

// A read-only directory can be read and not changed, in accept-edits mode
// too, and stays so after /clear; read-write lets accept-edits change it.
// Each is recorded with its access.
func TestAddDirAccess(t *testing.T) {
	t.Setenv("HOME", realDir(t))
	for _, access := range []string{accessRead, accessReadWrite} {
		t.Run(access, func(t *testing.T) {
			env, surface, events := permEnv(t, config.Default(), access)
			env.pol.Mode = policy.ModeAcceptEdits
			env.pol.Roots = env.sess.PolicyRoots
			dir := realDir(t)
			if _, err := slashAddDir(context.Background(), env, []string{dir}); err != nil {
				t.Fatal(err)
			}
			if len(surface.asked) != 1 || surface.asked[0].Default != ui.ChoiceNo {
				t.Fatalf("asked %+v", surface.asked)
			}
			target := filepath.Join(dir, "a.go")
			if _, err := env.sess.Resolve(target); err != nil {
				t.Fatalf("the directory is not reachable: %v", err)
			}
			env.st.fresh()
			write := env.pol.Evaluate("write", true, pathCall(target))
			edit := env.pol.Evaluate("edit", true, pathCall(target))
			read := env.pol.Evaluate("read", false, pathCall(target))
			if read.Decision != policy.Allow {
				t.Fatalf("read: %+v", read)
			}
			wantWrite := policy.Allow
			if access == accessRead {
				wantWrite = policy.Deny
			}
			if write.Decision != wantWrite || edit.Decision != wantWrite {
				t.Fatalf("%s: write %+v, edit %+v", access, write, edit)
			}
			added := eventsOf[agent.WorkspaceDirAdded](t, events(), agent.EvWorkspaceDirAdded)
			if len(added) != 1 || added[0].Canonical != dir || added[0].Access != access || added[0].By != agent.ByUser {
				t.Fatalf("recorded %+v", added)
			}
		})
	}
}

// No answer, or no, adds nothing and records nothing.
func TestAddDirUnansweredAddsNothing(t *testing.T) {
	t.Setenv("HOME", realDir(t))
	for _, answers := range [][]string{nil, {""}, {ui.ChoiceNo}} {
		env, _, events := permEnv(t, config.Default(), answers...)
		dir := realDir(t)
		if _, err := slashAddDir(context.Background(), env, []string{dir}); err != nil {
			t.Fatal(err)
		}
		if _, err := env.sess.Resolve(filepath.Join(dir, "a")); err == nil || len(events()) != 0 {
			t.Fatalf("answers %q: reachable (%v), %d events", answers, err, len(events()))
		}
	}
}

// A managed additional_dirs refuses /add-dir as it refuses the flag, before
// anyone is asked.
func TestAddDirIsRefusedUnderAManagedList(t *testing.T) {
	t.Setenv("HOME", realDir(t))
	cfg := config.Default()
	cfg.Managed, cfg.ManagedKeys, cfg.AdditionalDirs = true, []string{"additional_dirs"}, []string{"/srv/shared"}
	env, surface, events := permEnv(t, cfg, accessReadWrite)
	_, err := slashAddDir(context.Background(), env, []string{realDir(t)})
	var me *config.ManagedError
	if !errors.As(err, &me) || len(surface.asked) != 0 || len(events()) != 0 {
		t.Fatalf("err %v, asked %d, events %d", err, len(surface.asked), len(events()))
	}
}

// Abhed's state, credentials and a folder holding the home directory are
// refused, by where a link leads rather than what it is called; a link to an
// ordinary folder is shown with where it leads.
func TestAddDirRefusesStateAndCredentials(t *testing.T) {
	home := realDir(t)
	t.Setenv("HOME", home)
	for _, d := range []string{".abhed", ".ssh", "projects"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env, surface, _ := permEnv(t, config.Default(), accessRead, accessRead)
	link := filepath.Join(env.st.workspace, "docs")
	if err := os.Symlink(filepath.Join(home, ".ssh"), link); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"~/.abhed", filepath.Join(home, ".abhed"), link, filepath.Dir(home), home} {
		_, err := slashAddDir(context.Background(), env, []string{dir})
		if err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("%s: %v", dir, err)
		}
	}
	if len(surface.asked) != 0 {
		t.Fatalf("asked about a refused directory: %+v", surface.asked)
	}
	ok := filepath.Join(env.st.workspace, "proj")
	if err := os.Symlink(filepath.Join(home, "projects"), ok); err != nil {
		t.Fatal(err)
	}
	if _, err := slashAddDir(context.Background(), env, []string{ok}); err != nil {
		t.Fatal(err)
	}
	if len(surface.asked) != 1 || !strings.Contains(surface.asked[0].Body[0].Text, "resolves to "+filepath.Join(home, "projects")) {
		t.Fatalf("the dialog did not show where the link leads: %+v", surface.asked)
	}
	if _, err := slashAddDir(context.Background(), env, []string{env.st.workspace}); err == nil {
		t.Fatal("the workspace itself was added again")
	}
}

// Through the CLI: the directory reaches the record.
func TestCLIAddDirIsRecorded(t *testing.T) {
	c := startCLI(t)
	dir := realDir(t)
	c.command("/add-dir "+dir, "answer 1-3")
	c.command("1", "(read) for this session")
	c.task("hello")
	added := eventsOf[agent.WorkspaceDirAdded](t, c.export(), agent.EvWorkspaceDirAdded)
	if len(added) != 1 || added[0].Canonical != dir || added[0].Access != accessRead {
		t.Fatalf("recorded %+v", added)
	}
}
