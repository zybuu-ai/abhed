package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// A directory swapped for a link to ~/.ssh while the person answers is not
// added: what is added must be what was checked and shown.
func TestAddDirRefusesADirectorySwappedMidDialog(t *testing.T) {
	home := realDir(t)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_rsa"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, surface, events := permEnv(t, config.Default(), accessReadWrite)
	proj := filepath.Join(realDir(t), "proj")
	if err := os.Mkdir(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	surface.during = func() {
		if err := os.Remove(proj); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(home, ".ssh"), proj); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := slashAddDir(context.Background(), env, []string{proj}); err == nil {
		t.Fatal("the swapped directory was added")
	}
	if _, err := env.sess.Resolve(filepath.Join(home, ".ssh", "id_rsa")); err == nil {
		t.Fatal("~/.ssh became reachable")
	}
	if len(events()) != 0 {
		t.Fatalf("recorded %d events for a refused directory", len(events()))
	}
}

// Swapped for a link to an ordinary folder, the directory is still refused:
// the record and the read-only rules would name one folder and the root
// would be another.
func TestAddDirRefusesAnyDirectoryChangedMidDialog(t *testing.T) {
	t.Setenv("HOME", realDir(t))
	env, surface, events := permEnv(t, config.Default(), accessRead)
	base := realDir(t)
	proj, elsewhere := filepath.Join(base, "proj"), filepath.Join(base, "elsewhere")
	for _, d := range []string{proj, elsewhere} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	surface.during = func() {
		if err := os.Remove(proj); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, proj); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := slashAddDir(context.Background(), env, []string{proj}); err == nil {
		t.Fatal("a directory changed mid-dialog was added")
	}
	if _, err := env.sess.Resolve(filepath.Join(elsewhere, "a")); err == nil || len(events()) != 0 {
		t.Fatalf("the other folder became reachable (%v) or was recorded", err)
	}
}

// A folder whose name holds characters rules treat specially is kept
// read-only all the same; one with * or ?, which no rule can name exactly,
// is refused before anyone is asked.
func TestAddDirReadOnlyHoldsForAnyName(t *testing.T) {
	t.Setenv("HOME", realDir(t))
	for _, name := range []string{"a[b]", "c{d,e}", "f(g)", "h.i+j"} {
		env, _, _ := permEnv(t, config.Default(), accessRead)
		env.pol.Mode = policy.ModeAcceptEdits
		dir := filepath.Join(realDir(t), name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := slashAddDir(context.Background(), env, []string{dir}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := env.pol.Evaluate("write", true, pathCall(filepath.Join(dir, "x.go"))); got.Decision != policy.Deny {
			t.Errorf("%s: a read-only folder took a write: %+v", name, got)
		}
	}
	for _, name := range []string{"k*", "l?"} {
		env, surface, _ := permEnv(t, config.Default(), accessRead)
		dir := filepath.Join(realDir(t), name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := slashAddDir(context.Background(), env, []string{dir}); err == nil || len(surface.asked) != 0 {
			t.Errorf("%s: added (%v) or asked", name, err)
		}
	}
}

// A new conversation's record states what it inherits from earlier ones in
// the session: the mode and each added directory with its access, once
// each, before anything changed since.
func TestNewConversationRecordsWhatItInherits(t *testing.T) {
	t.Setenv("HOME", realDir(t))
	env, _, first := permEnv(t, config.Default(), accessRead)
	dir := realDir(t)
	if _, err := slashAddDir(context.Background(), env, []string{dir}); err != nil {
		t.Fatal(err)
	}
	if err := env.modes.Set(context.Background(), policy.ModePlan, agent.ViaSlash); err != nil {
		t.Fatal(err)
	}
	if n := len(first()); n != 2 {
		t.Fatalf("the first conversation recorded %d events", n)
	}
	// /clear, then a mode change before the next conversation opens.
	env.st.loop = nil
	env.st.fresh()
	if err := env.modes.Set(context.Background(), policy.ModeAcceptEdits, agent.ViaSlash); err != nil {
		t.Fatal(err)
	}
	store := env.st.store.(*agent.MemStore)
	env.st.loop = &agent.Loop{Recorder: agent.NewRecorder(store, "s2", ""), Policy: env.pol, Session: env.sess}
	env.st.flushPending()
	second, _ := store.Events("s2")
	if got := modeChanges(t, second); !slices.Equal(got, []string{"default>plan/carried", "plan>accept-edits/slash"}) {
		t.Fatalf("mode changes %v", got)
	}
	dirs := eventsOf[agent.WorkspaceDirAdded](t, second, agent.EvWorkspaceDirAdded)
	if len(dirs) != 1 || dirs[0].Canonical != dir || dirs[0].Access != accessRead {
		t.Fatalf("directories %+v", dirs)
	}
}

// /add-dir refuses the workspace's own .abhed, which only the person edits.
func TestAddDirRefusesTheWorkspaceState(t *testing.T) {
	t.Setenv("HOME", realDir(t))
	env, surface, _ := permEnv(t, config.Default(), accessRead)
	state := filepath.Join(env.st.workspace, ".abhed", "hooks")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := slashAddDir(context.Background(), env, []string{state}); err == nil || len(surface.asked) != 0 {
		t.Fatalf("the workspace's state was added (%v) or asked about", err)
	}
}
