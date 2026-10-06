package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitTree makes a repository's git folder by hand, with a submodule's git
// folder under modules and a linked worktree's under worktrees, each written
// once with mark so a change shows.
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

// With ProtectGit, no command can write a commondir, a submodule's config or
// hooks, a config.worktree or a linked worktree's pointers: seatbelt refuses
// each write, even of a file that did not exist; bubblewrap binds what exists
// read-only and takes out a commondir before the next command runs.
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

// The container binds a submodule's config and hooks and a worktree's
// commondir read-only, and refuses to run while a commondir is planted,
// taking it out to the quarantine.
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
