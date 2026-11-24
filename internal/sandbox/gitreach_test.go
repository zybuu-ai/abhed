package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitShapes makes, under base, a repository and the three workspaces whose
// git folder lies outside them: a folder inside it, a linked worktree, and a
// submodule checkout. It returns the repository's top and each workspace.
func gitShapes(t *testing.T, base string) (repo string, shapes map[string]string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo = filepath.Join(base, "repo")
	gitRun(t, base, "init", "-q", "-b", "main", repo)
	if err := os.MkdirAll(filepath.Join(repo, "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	for f, body := range map[string]string{"README.md": "top readme\n", "deep/a.txt": "a\n"} {
		if err := os.WriteFile(filepath.Join(repo, f), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "first")
	wt := filepath.Join(base, "wt")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "wt", wt)
	super := filepath.Join(base, "super")
	gitRun(t, base, "init", "-q", "-b", "main", super)
	gitRun(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", repo, "app")
	return repo, map[string]string{
		"subfolder":       filepath.Join(repo, "deep"),
		"linked worktree": wt,
		"submodule":       filepath.Join(super, "app"),
	}
}

// GitReach names the repository's top for a folder inside it, and the git
// folders a worktree's or a submodule's .git file names, and nothing else.
func TestGitReachNamesWhatGitReads(t *testing.T) {
	base := workspace(t)
	repo, shapes := gitShapes(t, base)
	want := map[string][]string{
		"subfolder":       {repo, filepath.Join(repo, ".git")},
		"linked worktree": {filepath.Join(repo, ".git", "worktrees", "wt"), filepath.Join(repo, ".git")},
		"submodule":       {filepath.Join(base, "super", ".git", "modules", "app")},
	}
	for name, ws := range shapes {
		if got := GitReach(ws); !slices.Equal(got, want[name]) {
			t.Errorf("%s: GitReach = %v, want %v", name, got, want[name])
		}
	}
	if got := GitReach(repo); len(got) != 0 {
		t.Errorf("a repository's top reaches %v", got)
	}
	if got := GitReach(filepath.Join(base, "nothing")); len(got) != 0 {
		t.Errorf("a folder in no repository reaches %v", got)
	}
}

// A .git file a command rewrote to name another repository's git folder, or
// a submodule's git folder naming another work tree, reaches nothing; nor
// does a repository that is a home kept in git.
func TestGitReachRefusesWhatDoesNotNameTheWorktreeBack(t *testing.T) {
	base := workspace(t)
	_, shapes := gitShapes(t, base)
	other := filepath.Join(base, "other")
	gitRun(t, base, "init", "-q", "-b", "main", other)
	wt := shapes["linked worktree"]
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(other, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := GitReach(wt); len(got) != 0 {
		t.Errorf("a .git file naming another repository reaches %v", got)
	}
	app := shapes["submodule"]
	gitRun(t, filepath.Join(base, "super", ".git", "modules", "app"), "config", "core.worktree", other)
	if got := GitReach(app); len(got) != 0 {
		t.Errorf("a submodule git folder naming another work tree reaches %v", got)
	}
	// A home kept in git: its git folder would show the home.
	t.Setenv("HOME", filepath.Join(base, "repo"))
	if got := GitReach(shapes["subfolder"]); len(got) != 0 {
		t.Errorf("a repository whose top is a home reaches %v", got)
	}
}

// reachSandbox is the process tier for ws, with git's files held as every
// session holds them.
func reachSandbox(t *testing.T, ws string) *Process {
	t.Helper()
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// gitFolders are each shape's git folder and common git folder.
func gitFolders(base, repo, shape string) (gd, common string) {
	switch shape {
	case "linked worktree":
		return filepath.Join(repo, ".git", "worktrees", "wt"), filepath.Join(repo, ".git")
	case "submodule":
		g := filepath.Join(base, "super", ".git", "modules", "app")
		return g, g
	}
	return filepath.Join(repo, ".git"), filepath.Join(repo, ".git")
}

// On the process tier, git works in a workspace whose git folder lies
// outside it, even under a home the sandbox otherwise keeps unreadable: it
// reads the repository's top (for a folder inside it) and commits into the
// git folders, as it does in one inside the workspace. What git reads as
// configuration or runs there stays as unwritable as in the workspace's own.
func TestProcessSandboxReachesAGitFolderOutsideTheWorkspace(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the process tier runs on macOS and Linux")
	}
	base := workspace(t)
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// The repositories live in a home, which the sandbox keeps unreadable but
	// for the workspace: as a person's projects under ~ are.
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo, shapes := gitShapes(t, home)
	for name, ws := range shapes {
		t.Run(name, func(t *testing.T) {
			s := reachSandbox(t, ws)
			if out, err := runIn(t, s, ws, "git status --short && git log -1 --format=%s"); err != nil || !strings.Contains(out, "first") {
				t.Fatalf("git in the sandbox: %v\n%s", err, out)
			}
			if name == "subfolder" {
				if out, err := runIn(t, s, ws, "cat ../README.md"); err != nil || !strings.Contains(out, "top readme") {
					t.Fatalf("the repository's top was not readable: %v\n%s", err, out)
				}
			}
			commit := "echo x > made-in-sandbox.txt && git add made-in-sandbox.txt && " +
				"git -c user.name=t -c user.email=t@example.test commit -q -m in-sandbox && git log -1 --format=%s"
			if out, err := runIn(t, s, ws, commit); err != nil || !strings.Contains(out, "in-sandbox") {
				t.Fatalf("a commit in the sandbox: %v\n%s", err, out)
			}
			gd, common := gitFolders(home, repo, name)
			for _, attempt := range []string{
				"mkdir -p " + filepath.Join(common, "hooks") + " && echo 'exit 0' > " + filepath.Join(common, "hooks", "pre-commit"),
				"echo '[core]' >> " + filepath.Join(common, "config"),
				"git config core.hooksPath " + ws,
				"mv " + filepath.Join(common, "hooks") + " " + filepath.Join(common, "hooks-moved"),
			} {
				if out, err := runIn(t, s, ws, attempt); err == nil {
					t.Errorf("the sandbox let a command run %q: %s", attempt, out)
				}
			}
			// A commondir pointing git elsewhere: refused by pattern on macOS;
			// bubblewrap holds only what exists, so one made in a repository's
			// git folder is taken out before the next command, as in the
			// workspace's own git folder.
			planted := filepath.Join(gd, "commondir")
			_, had := os.Lstat(planted)
			_, _ = runIn(t, s, ws, "echo /elsewhere > "+planted)
			_, _ = runIn(t, s, ws, "true")
			if b, err := os.ReadFile(planted); err == nil && strings.Contains(string(b), "elsewhere") {
				t.Error("a commondir planted in the git folder outside the workspace stayed")
			} else if had == nil && err != nil {
				t.Error("the worktree's own commondir was taken out")
			}
			if out, err := runIn(t, s, ws, "git log -1 --format=%s"); err != nil || !strings.Contains(out, "in-sandbox") {
				t.Errorf("git after the plant was taken out: %v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(common, "hooks", "pre-commit")); err == nil {
				t.Error("a hook was planted in the git folder outside the workspace")
			}
			if b, _ := os.ReadFile(filepath.Join(common, "config")); strings.Contains(string(b), "hooksPath") {
				t.Error("the git folder's configuration was written")
			}
			// The repository's top's own files stay unwritten. On macOS the test's
			// temp folder is writable to commands anyway, so the rule is checked.
			if name == "subfolder" {
				if s.backend == "sandbox-exec" {
					for _, f := range PathForms(repo) {
						if strings.Contains(s.seatbeltProfile(), "(allow file-write* (subpath "+quote(f)+"))") {
							t.Error("the profile lets commands write the repository's top")
						}
					}
				} else if _, err := runIn(t, s, ws, "echo x >> ../README.md"); err == nil {
					t.Error("a command wrote the repository's top")
				}
			}
			if n := s.gitReachNote(); n == "" {
				t.Error("the description does not say what outside the workspace git reaches")
			}
		})
	}
}

// Reading a repository's top never reaches Abhed state in it: a top that
// holds another workspace's .abhed is not read at all (only its git folder),
// and on macOS one made later, in any case, stays unreadable.
func TestTheRepositoryTopKeepsOtherStateOut(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the process tier runs on macOS and Linux")
	}
	base := workspace(t)
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo, shapes := gitShapes(t, home)
	ws := shapes["subfolder"]
	other := filepath.Join(repo, "other", stateDir)
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "record"), []byte("NESTED-STATE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := reachSandbox(t, ws)
	if out, err := runIn(t, s, ws, "cat ../other/.abhed/record"); err == nil || strings.Contains(out, "NESTED-STATE") {
		t.Fatalf("another workspace's state was read: %s", out)
	}
	if r := s.reach(); r.top != "" {
		t.Errorf("a top holding another .abhed was read: %+v", r)
	}
	if out, err := runIn(t, s, ws, "git log -1 --format=%s"); err != nil || !strings.Contains(out, "first") {
		t.Errorf("git log without the top: %v\n%s", err, out)
	}

	if runtime.GOOS != "darwin" {
		return
	}
	// Made after the sandbox read the top: denied by pattern, in any case.
	if err := os.RemoveAll(filepath.Join(repo, "other")); err != nil {
		t.Fatal(err)
	}
	s2 := reachSandbox(t, ws)
	if _, err := runIn(t, s2, ws, "true"); err != nil {
		t.Fatal(err)
	}
	if r := s2.reach(); r.top == "" {
		t.Fatal("the top was not read")
	}
	later := filepath.Join(repo, "other", ".ABHED")
	if err := os.MkdirAll(later, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(later, "record"), []byte("NESTED-STATE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../other/.ABHED/record", "../OTHER/.abhed/record", "../other/.abhed/record"} {
		if out, err := runIn(t, s2, ws, "cat "+p); err == nil || strings.Contains(out, "NESTED-STATE") {
			t.Errorf("%s was read: %s", p, out)
		}
	}
}
