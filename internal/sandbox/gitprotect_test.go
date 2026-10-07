package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// gitTree makes a git folder by hand, with a submodule's and a linked
// worktree's, writing mark into each so a change shows.
func gitTree(t *testing.T, ws, mark string) {
	t.Helper()
	files := map[string]string{
		".git/HEAD":                      "ref: refs/heads/main\n",
		".git/config":                    "[core]\n[extensions]\n\tworktreeConfig = true\n",
		".git/modules/lib/x/HEAD":        "ref: refs/heads/main\n",
		".git/modules/lib/x/config":      mark,
		".git/modules/lib/x/hooks/.keep": "",
		".git/worktrees/w/HEAD":          "ref: refs/heads/w\n",
		".git/worktrees/w/commondir":     "../..\n",
		".git/worktrees/w/gitdir":        mark,
		".git/info/attributes":           mark,
		".git/objects/info/alternates":   mark,
	}
	for f, data := range files {
		p := filepath.Join(ws, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{".git/hooks", ".git/refs/heads", ".git/objects/pack"} {
		if err := os.MkdirAll(filepath.Join(ws, filepath.FromSlash(d)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
}

// gitPlant is a command that tries every way of pointing git at
// configuration or hooks a command controls.
const gitPlant = "mkdir -p evil/hooks; printf '[core]\\n\\tfsmonitor = touch marker\\n' > evil/config; " +
	"echo $PWD/evil > .git/commondir; echo $PWD/evil > .git/modules/lib/x/commondir; " +
	"echo planted >> .git/modules/lib/x/config; echo planted > .git/modules/lib/x/hooks/post-checkout; " +
	"mkdir -p .git/modules/new/hooks; echo planted > .git/modules/new/config; echo planted > .git/modules/new/HEAD; " +
	"echo planted > .git/config.worktree; echo planted > .git/worktrees/w/config.worktree; echo /x > .git/worktrees/w/commondir; " +
	"echo planted > .git/worktrees/w/gitdir; echo planted >> .git/info/attributes; echo /x >> .git/objects/info/alternates"

// checkGitPlant fails for each pointer the plant left in place.
func checkGitPlant(t *testing.T, ws, mark string) {
	t.Helper()
	for _, f := range []string{".git/commondir", ".git/modules/lib/x/commondir", ".git/modules/lib/x/hooks/post-checkout"} {
		if _, err := os.Lstat(filepath.Join(ws, filepath.FromSlash(f))); err == nil {
			t.Errorf("the command made %s", f)
		}
	}
	unchanged := map[string]string{
		".git/modules/lib/x/config":        mark,
		".git/config.worktree":             "",
		".git/worktrees/w/config.worktree": "",
		".git/worktrees/w/commondir":       "../..\n",
		".git/worktrees/w/gitdir":          mark,
		".git/info/attributes":             mark,
		".git/objects/info/alternates":     mark,
	}
	for f, want := range unchanged {
		data, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(f)))
		if (err != nil && want != "") || string(data) != want {
			t.Errorf("the command changed %s: %q (%v)", f, data, err)
		}
	}
}

// With ProtectGit, no command can write any git pointer: seatbelt refuses each
// write; bubblewrap binds them and takes out a commondir before the next command.
func TestProcessSandboxProtectsEveryGitPointer(t *testing.T) {
	requireNetNS(t)
	t.Setenv("HOME", t.TempDir())
	ws := workspace(t)
	const mark = "[core]\n\tbare = false\n"
	gitTree(t, ws, mark)
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	available(t, s)
	_, _ = runIn(t, s, ws, gitPlant)
	// Bubblewrap cannot stop a commondir being made; the next command finds it.
	out, err := runIn(t, s, ws, "true")
	if s.Backend() == "bwrap" && (err == nil || !strings.Contains(err.Error()+out, "commondir")) {
		t.Errorf("a planted commondir did not stop the next command: %v %s", err, out)
	}
	checkGitPlant(t, ws, mark)
	if out, err := runIn(t, s, ws, "echo ok > .git/HEAD && echo ok > .git/modules/lib/x/HEAD && touch .git/objects/pack/p .git/worktrees/w/HEAD"); err != nil {
		t.Fatalf("the rest of the git folders is not writable: %v %s", err, out)
	}
}

// Git itself still works in the sandbox with ProtectGit: add, commit,
// branch, checkout and a fetch into the existing repository.
func TestProcessSandboxGitWorksWithProtectGit(t *testing.T) {
	requireNetNS(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	ws := workspace(t)
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", filepath.Join(ws, "repo")},
		{"init", "-q", "-b", "main", filepath.Join(ws, "other")},
		{"-C", filepath.Join(ws, "other"), "-c", "user.name=a", "-c", "user.email=a@b", "commit", "-q", "--allow-empty", "-m", "other"},
	} {
		if out, err := exec.Command(git, args...).CombinedOutput(); err != nil { // #nosec G204 -- test fixture
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	available(t, s)
	g := "git -c user.name=a -c user.email=a@b "
	out, err := runIn(t, s, filepath.Join(ws, "repo"), "set -e; echo hi > f; "+g+"add f; "+g+"commit -q -m one; "+
		g+"branch b; "+g+"checkout -q b; echo two >> f; "+g+"commit -q -am two; "+g+"checkout -q main; "+
		g+"fetch -q ../other main:other; "+g+"log --oneline other | grep -q other; git status --short")
	if err != nil {
		t.Fatalf("git in the sandbox: %v\n%s", err, out)
	}
}

// The walk finds a submodule's git folder under a name with slashes, and a
// linked worktree's, with what git reads in each.
func TestGitProtectedFindsSubmodulesAndWorktrees(t *testing.T) {
	ws := workspace(t)
	gitTree(t, ws, "x")
	got := map[string]bool{}
	for _, p := range GitProtected(ws) {
		rel, _ := filepath.Rel(ws, p)
		got[filepath.ToSlash(rel)] = true
	}
	for _, want := range []string{".git/config", ".git/hooks", ".git/config.worktree", ".git/info/attributes",
		".git/objects/info/alternates", ".git/modules/lib/x/config", ".git/modules/lib/x/hooks",
		".git/worktrees/w/commondir", ".git/worktrees/w/gitdir", ".git/worktrees/w/config.worktree"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
}

// Without extensions.worktreeConfig no config.worktree is made: git would
// not read one, and the repository is left as it was.
func TestGitProtectedMakesNoNeedlessConfigWorktree(t *testing.T) {
	ws := workspace(t)
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	GitProtected(ws)
	if _, err := os.Lstat(filepath.Join(ws, ".git", "config.worktree")); err == nil {
		t.Fatal("made a config.worktree git does not read")
	}
}

// The container binds the git pointers read-only, and refuses to run while a
// commondir is planted, quarantining it.
func TestContainerProtectsEveryGitPointer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitTree(t, ws, "x")
	c := &Container{policy: Policy{Workspace: ws, ProtectGit: true}}
	got := strings.Join(c.Command(context.Background(), ws, "true").Args, " ")
	for _, f := range []string{".git/modules/lib/x/config", ".git/modules/lib/x/hooks", ".git/worktrees/w/commondir", ".git/config.worktree"} {
		p := filepath.Join(ws, filepath.FromSlash(f))
		if !strings.Contains(got, "-v "+p+":"+p+":ro") {
			t.Errorf("%s not bound read-only:\n%s", f, got)
		}
	}
	planted := filepath.Join(ws, ".git", "commondir")
	build := []func() *exec.Cmd{
		func() *exec.Cmd { return c.Command(context.Background(), ws, "true") },
		func() *exec.Cmd { return c.Shell(context.Background(), ws) },
	}
	for _, b := range build {
		if err := os.WriteFile(planted, []byte("/elsewhere\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if cmd := b(); cmd.Err == nil || !strings.Contains(cmd.Err.Error(), "commondir") {
			t.Errorf("ran with a planted commondir: %v", cmd.Err)
		}
		if _, err := os.Lstat(planted); err == nil {
			t.Error("the planted commondir is still there")
		}
	}
	// What was taken out is kept in the quarantine, not deleted.
	home, _ := os.UserHomeDir()
	if m, _ := filepath.Glob(filepath.Join(home, stateDir, QuarantineDir, "git-*", "commondir")); len(m) == 0 {
		t.Error("the planted commondir is not in the quarantine")
	}
}

// writeFiles writes each file under ws, making its folders.
func writeFiles(t *testing.T, ws string, files map[string]string) {
	t.Helper()
	for f, data := range files {
		p := filepath.Join(ws, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func relSet(ws string, paths []string) map[string]bool {
	got := map[string]bool{}
	for _, p := range paths {
		rel, _ := filepath.Rel(ws, p)
		got[filepath.ToSlash(rel)] = true
	}
	return got
}

// Folders past the walk's bound, sorting before .git, hide no own git folder,
// folder's .git, submodule's or linked worktree's .git file.
func TestScanGitFindsEachGitBeforeTheBound(t *testing.T) {
	old := gitWalkFolders
	gitWalkFolders = 20
	t.Cleanup(func() { gitWalkFolders = old })
	ws := workspace(t)
	writeFiles(t, ws, map[string]string{
		".git/HEAD":               "ref: refs/heads/main\n",
		".git/config":             "[core]\n",
		".git/modules/z/HEAD":     "ref: refs/heads/main\n",
		".git/modules/z/config":   "[core]\n\tworktree = ../../../z\n",
		".git/worktrees/w/HEAD":   "ref: refs/heads/w\n",
		".git/worktrees/w/gitdir": filepath.Join(ws, "w", ".git") + "\n",
		"-a/.git/HEAD":            "ref: refs/heads/main\n",
		"-a/.git/config":          "[core]\n",
		"z/.git":                  "gitdir: ../.git/modules/z\n",
		"w/.git":                  "gitdir: " + filepath.Join(ws, ".git", "worktrees", "w") + "\n",
	})
	for i := range 3 * gitWalkFolders {
		if err := os.MkdirAll(filepath.Join(ws, "-a", "-b", strconv.Itoa(i)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	g := scanGit(ws)
	if !g.walkBounded {
		t.Fatal("the walk did not stop at its bound")
	}
	got := relSet(ws, g.protected)
	for _, want := range []string{".git/config", ".git/modules/z/config", ".git/worktrees/w/gitdir", "-a/.git/config", "z/.git", "w/.git"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
}

// A linked worktree's commondir is git's while it names the repository's
// git folder; one changed to name anything else counts as planted.
func TestWorktreeCommondirPointingElsewhereIsPlanted(t *testing.T) {
	ws := workspace(t)
	gitTree(t, ws, "x")
	if g := scanGit(ws); len(g.planted) != 0 {
		t.Fatalf("git's own commondir counted as planted: %v", g.planted)
	}
	c := filepath.Join(ws, ".git", "worktrees", "w", "commondir")
	for _, to := range []string{filepath.Join(ws, "evil") + "\n", "../../../evil\n", ""} {
		if err := os.WriteFile(c, []byte(to), 0o600); err != nil {
			t.Fatal(err)
		}
		if g := scanGit(ws); len(g.planted) != 1 || g.planted[0] != c {
			t.Errorf("commondir %q not planted: %v", to, g.planted)
		}
	}
}

// A commondir reaching the repository through a link counts as planted: the
// link could be pointed at another git folder once the commondir is bound.
func TestWorktreeCommondirThroughALinkIsPlanted(t *testing.T) {
	ws := workspace(t)
	gitTree(t, ws, "x")
	if err := os.Symlink(".", filepath.Join(ws, ".git", "lnk")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".git", filepath.Join(ws, "gl")); err != nil {
		t.Fatal(err)
	}
	// Git reads the whole file, so a second line names a folder of its own.
	if err := os.MkdirAll(filepath.Join(ws, ".git", "worktrees", "..\nx"), 0o750); err != nil {
		t.Fatal(err)
	}
	c := filepath.Join(ws, ".git", "worktrees", "w", "commondir")
	for _, to := range []string{"../../lnk\n", filepath.Join(ws, "gl") + "\n", "../..\nx\n", "../../../.git\n"} {
		if err := os.WriteFile(c, []byte(to), 0o600); err != nil {
			t.Fatal(err)
		}
		if g := scanGit(ws); len(g.planted) != 1 || g.planted[0] != c {
			t.Errorf("commondir %q not planted: %v", to, g.planted)
		}
	}
	for _, to := range []string{"../..\n", "../../\n", filepath.Join(ws, ".git") + "\n"} {
		if err := os.WriteFile(c, []byte(to), 0o600); err != nil {
			t.Fatal(err)
		}
		if g := scanGit(ws); len(g.planted) != 0 {
			t.Errorf("commondir %q counted as planted: %v", to, g.planted)
		}
	}
}

// The commondir `git worktree add` writes is git's own.
func TestGitWorktreeAddCommondirIsNotPlanted(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	t.Setenv("HOME", t.TempDir())
	ws := workspace(t)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", ws},
		{"-C", ws, "-c", "user.name=a", "-c", "user.email=a@b", "commit", "-q", "--allow-empty", "-m", "one"},
		{"-C", ws, "worktree", "add", "-q", "-b", "w", filepath.Join(ws, "wt")},
	} {
		if out, err := exec.Command(git, args...).CombinedOutput(); err != nil { // #nosec G204 -- test fixture
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	c := filepath.Join(ws, ".git", "worktrees", "wt", "commondir")
	if !exists(c) {
		t.Fatal("git wrote no commondir")
	}
	g := scanGit(ws)
	if len(g.planted) != 0 {
		t.Fatalf("git's own commondir counted as planted: %v", g.planted)
	}
	if got := relSet(ws, g.protected); !got[".git/worktrees/wt/commondir"] || !got["wt/.git"] {
		t.Errorf("the linked worktree is not protected: %v", got)
	}
}

// A nested repository found by an earlier scan stays protected after a
// command floods the workspace with folders sorting before it.
func TestGitGuardKeepsRepositoriesFoundEarlier(t *testing.T) {
	old := gitWalkFolders
	gitWalkFolders = 20
	t.Cleanup(func() { gitWalkFolders = old })
	ws := workspace(t)
	writeFiles(t, ws, map[string]string{
		".git/HEAD": "ref: refs/heads/main\n", ".git/config": "[core]\n",
		"zz/.git/HEAD": "ref: refs/heads/main\n", "zz/.git/config": "[core]\n",
	})
	var rec recorded
	var mem gitMemory
	found, err := gitGuard(rec.ctx(), ws, "bwrap", &mem)
	if err != nil || !relSet(ws, found)["zz/.git/config"] {
		t.Fatalf("the nested repository was not found: %v %v", err, found)
	}
	for i := range 3 * gitWalkFolders {
		if err := os.MkdirAll(filepath.Join(ws, "a"+strconv.Itoa(i)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if relSet(ws, scanGit(ws).protected)["zz/.git/config"] {
		t.Fatal("the flood did not hide the nested repository from a fresh walk")
	}
	found, err = gitGuard(rec.ctx(), ws, "bwrap", &mem)
	if err != nil || !relSet(ws, found)["zz/.git/config"] {
		t.Fatalf("the flood unprotected a repository found earlier: %v %v", err, relSet(ws, found))
	}
	if got := rec.of(EvGitWalkBounded); len(got) != 1 {
		t.Errorf("the bound was not recorded: %v", rec.events)
	}
	// One whose folder became a link is not followed out of the workspace.
	out := t.TempDir()
	writeFiles(t, out, map[string]string{".git/HEAD": "ref: refs/heads/main\n"})
	if err := os.RemoveAll(filepath.Join(ws, "zz")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, filepath.Join(ws, "zz")); err != nil {
		t.Fatal(err)
	}
	if found, err = gitGuard(rec.ctx(), ws, "bwrap", &mem); err != nil || exists(filepath.Join(out, ".git", "config")) {
		t.Fatalf("followed a link out of the workspace: %v %v", err, found)
	}
}

// recorded collects the events a sandbox writes to a call's record.
type recorded struct{ events []map[string]any }

func (r *recorded) ctx() context.Context {
	return WithLaunch(context.Background(), Launch{CallID: "call-1", Record: func(ev string, pay map[string]any) error {
		pay["event"] = ev
		r.events = append(r.events, pay)
		return nil
	}})
}

func (r *recorded) of(ev string) []map[string]any {
	var out []map[string]any
	for _, e := range r.events {
		if e["event"] == ev {
			out = append(out, e)
		}
	}
	return out
}

// A planted commondir taken out is recorded with the call, saying where it
// went, and the command is refused with the reason.
func TestGitPlantedIsRecorded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := workspace(t)
	gitTree(t, ws, "x")
	planted := filepath.Join(ws, ".git", "commondir")
	if err := os.WriteFile(planted, []byte("/elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var rec recorded
	c := &Container{policy: Policy{Workspace: ws, ProtectGit: true}}
	if cmd := c.Command(rec.ctx(), ws, "true"); cmd.Err == nil || !strings.Contains(cmd.Err.Error(), "commondir") {
		t.Fatalf("ran with a planted commondir: %v", cmd.Err)
	}
	got := rec.of(EvGitPlanted)
	if len(got) != 1 || got[0]["call_id"] != "call-1" || got[0]["workspace"] != ws {
		t.Fatalf("not recorded: %v", rec.events)
	}
	entries, _ := got[0]["entries"].([]map[string]any)
	if len(entries) != 1 || entries[0]["path"] != planted || entries[0]["outcome"] != plantMoved || entries[0]["moved_to"] == "" {
		t.Errorf("entries: %v", got[0]["entries"])
	}
}

// Stopping at the walk's bound is recorded once for the sandbox, and the
// command runs; modules holding more than their bound refuse it.
func TestGitWalkBoundIsRecorded(t *testing.T) {
	oldWalk, oldModules := gitWalkFolders, gitModulesEntries
	gitWalkFolders, gitModulesEntries = 5, 10
	t.Cleanup(func() { gitWalkFolders, gitModulesEntries = oldWalk, oldModules })
	ws := workspace(t)
	gitTree(t, ws, "x")
	for i := range 10 {
		if err := os.MkdirAll(filepath.Join(ws, "-a", strconv.Itoa(i)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	var rec recorded
	var mem gitMemory
	for range 2 {
		if _, err := gitGuard(rec.ctx(), ws, "bwrap", &mem); err != nil {
			t.Fatalf("refused at the walk's bound: %v", err)
		}
	}
	if got := rec.of(EvGitWalkBounded); len(got) != 1 || got[0]["refused"] != false {
		t.Fatalf("the bound was not recorded once: %v", rec.events)
	}
	for i := range 2 * gitModulesEntries {
		if err := os.MkdirAll(filepath.Join(ws, ".git", "modules", "-"+strconv.Itoa(i)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitGuard(rec.ctx(), ws, "bwrap", &mem); err == nil || !strings.Contains(err.Error(), "modules") {
		t.Fatalf("ran with modules past their bound: %v", err)
	}
	if got := rec.of(EvGitWalkBounded); len(got) != 2 || got[1]["refused"] != true {
		t.Fatalf("the refusal was not recorded: %v", rec.events)
	}
}

// A linked hooks folder cannot be bound and could be repointed, so bubblewrap
// and the container refuse to run rather than fail to start or leave it writable.
func TestLinkedGitHooksRefuseTheCommand(t *testing.T) {
	ws := workspace(t)
	writeFiles(t, ws, map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".git/config": "[core]\n", "hooks-real/pre-commit": "#!/bin/sh\n"})
	if err := os.Symlink("../hooks-real", filepath.Join(ws, ".git", "hooks")); err != nil {
		t.Fatal(err)
	}
	var mem gitMemory
	if _, err := gitGuard(context.Background(), ws, "bwrap", &mem); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a linked hooks folder did not refuse: %v", err)
	}
	c := &Container{policy: Policy{Workspace: ws, ProtectGit: true}}
	if cmd := c.Command(context.Background(), ws, "true"); cmd.Err == nil || !strings.Contains(cmd.Err.Error(), "symbolic link") {
		t.Fatalf("the container ran with a linked hooks folder: %v", cmd.Err)
	}
}

// A link where git reads a git folder or one of its parts cannot be bound,
// and could be repointed, so it refuses the command as a linked hooks does.
func TestLinkedGitFoldersRefuseTheCommand(t *testing.T) {
	cases := map[string]func(t *testing.T, ws string){
		// A worktree's folder moved aside for a link, its commondir naming another git folder.
		"worktree folder": func(t *testing.T, ws string) {
			gitTree(t, ws, "x")
			writeFiles(t, ws, map[string]string{"evilc/HEAD": "ref: refs/heads/main\n", "evilc/config": "[core]\n\tfsmonitor = x\n"})
			if err := os.Rename(filepath.Join(ws, ".git", "worktrees", "w"), filepath.Join(ws, "hid")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../../hid", filepath.Join(ws, ".git", "worktrees", "w")); err != nil {
				t.Fatal(err)
			}
			writeFiles(t, ws, map[string]string{"hid/commondir": filepath.Join(ws, "evilc") + "\n"})
		},
		"modules entry": func(t *testing.T, ws string) {
			gitTree(t, ws, "x")
			writeFiles(t, ws, map[string]string{"hidm/HEAD": "ref: refs/heads/main\n", "hidm/config": "[core]\n"})
			if err := os.Symlink("../../../../hidm", filepath.Join(ws, ".git", "modules", "lib", "y")); err != nil {
				t.Fatal(err)
			}
		},
		"modules folder": func(t *testing.T, ws string) {
			writeFiles(t, ws, map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".git/config": "[core]\n", "hidm/a/HEAD": "ref: refs/heads/main\n"})
			if err := os.Symlink("../hidm", filepath.Join(ws, ".git", "modules")); err != nil {
				t.Fatal(err)
			}
		},
		"workspace .git": func(t *testing.T, ws string) {
			writeFiles(t, ws, map[string]string{"real/HEAD": "ref: refs/heads/main\n", "real/config": "[core]\n"})
			if err := os.Symlink("real", filepath.Join(ws, ".git")); err != nil {
				t.Fatal(err)
			}
		},
		"nested .git": func(t *testing.T, ws string) {
			writeFiles(t, ws, map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".git/config": "[core]\n",
				"real/HEAD": "ref: refs/heads/main\n", "real/config": "[core]\n", "n/README": ""})
			if err := os.Symlink("../real", filepath.Join(ws, "n", ".git")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			ws := workspace(t)
			setup(t, ws)
			var mem gitMemory
			if _, err := gitGuard(context.Background(), ws, "bwrap", &mem); err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("a linked git folder did not refuse: %v", err)
			}
			c := &Container{policy: Policy{Workspace: ws, ProtectGit: true}}
			if cmd := c.Command(context.Background(), ws, "true"); cmd.Err == nil || !strings.Contains(cmd.Err.Error(), "symbolic link") {
				t.Fatalf("the container ran with a linked git folder: %v", cmd.Err)
			}
		})
	}
}

// The folders holding the pointers are matched themselves, at any depth
// and in any case, and nothing they hold is.
func TestSeatbeltGitFoldersPattern(t *testing.T) {
	ws := "/w/s"
	re := regexp.MustCompile(gitFoldersPattern(ws))
	for _, p := range []string{".git/modules", ".git/modules/x", ".GIT/Modules/X", "a/b/.git/worktrees", ".git/worktrees/w",
		".git/info", ".git/objects", ".git/objects/info", ".git/modules/a/modules/b", ".git/modules/a/b/info", ".git/worktrees/w/info"} {
		if !re.MatchString(ws + "/" + p) {
			t.Errorf("%s is not held", p)
		}
	}
	for _, p := range []string{".git/modules/x/HEAD", ".git/info/exclude", ".git/objects/ab", ".git/objects/info/packs",
		".git/worktrees/w/HEAD", ".git/refs", "info", "src/objects", ".git/modules/x/refs/heads"} {
		if re.MatchString(ws + "/" + p) {
			t.Errorf("%s is held", p)
		}
	}
}

// Folders past the walk's bound leave the repository protected on every
// backend: config unwritable, and a planted commondir stops the next command.
func TestProcessSandboxWalkBoundKeepsTheRepository(t *testing.T) {
	requireNetNS(t)
	t.Setenv("HOME", t.TempDir())
	old := gitWalkFolders
	gitWalkFolders = 20
	t.Cleanup(func() { gitWalkFolders = old })
	ws := workspace(t)
	const mark = "[core]\n\tbare = false\n"
	gitTree(t, ws, mark)
	writeFiles(t, ws, map[string]string{".git/config": mark})
	for i := range 3 * gitWalkFolders {
		if err := os.MkdirAll(filepath.Join(ws, "-a", strconv.Itoa(i)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	available(t, s)
	_, _ = runIn(t, s, ws, "echo '[core] fsmonitor = touch x' >> .git/config; echo $PWD/evil > .git/commondir")
	if data, _ := os.ReadFile(filepath.Join(ws, ".git", "config")); string(data) != mark {
		t.Errorf("the command changed .git/config: %q", data)
	}
	out, err := runIn(t, s, ws, "true")
	if s.Backend() == "bwrap" && (err == nil || !strings.Contains(err.Error()+out, "commondir")) {
		t.Errorf("a planted commondir did not stop the next command: %v %s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(ws, ".git", "commondir")); err == nil {
		t.Error("the planted commondir is still there")
	}
}

// Under bubblewrap a linked hooks folder refuses the command, saying why,
// rather than every command failing to start with a mount error.
func TestBwrapRefusesLinkedHooksClearly(t *testing.T) {
	requireNetNS(t)
	ws := workspace(t)
	writeFiles(t, ws, map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".git/config": "[core]\n", "hooks-real/.keep": ""})
	if err := os.Symlink("../hooks-real", filepath.Join(ws, ".git", "hooks")); err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	available(t, s)
	if s.Backend() != "bwrap" {
		t.Skip("seatbelt holds where the link leads; see the darwin tests")
	}
	out, err := runIn(t, s, ws, "true")
	if err == nil || !strings.Contains(err.Error()+out, "symbolic link") {
		t.Fatalf("not refused clearly: %v %s", err, out)
	}
}
