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
		"subfolder":       {repo},
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

// On the process tier, git works in a workspace whose git folder lies
// outside it, even under a home the sandbox otherwise keeps unreadable: the
// repository's top, the worktree's and the submodule's git folders are read.
// They are never written: the profile grants no write there, and under
// bubblewrap they are bound read-only.
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
			s := processSandbox(t, ws, false)
			t.Cleanup(func() { _ = s.(interface{ Close() error }).Close() })
			if out, err := runIn(t, s, ws, "git status --short && git log -1 --format=%s"); err != nil || !strings.Contains(out, "first") {
				t.Fatalf("git in the sandbox: %v\n%s", err, out)
			}
			if name == "subfolder" {
				if out, err := runIn(t, s, ws, "cat ../README.md"); err != nil || !strings.Contains(out, "top readme") {
					t.Fatalf("the repository's top was not readable: %v\n%s", err, out)
				}
			}
			p := s.(*Process)
			reach := p.gitReads()
			if len(reach) == 0 {
				t.Fatal("nothing outside the workspace was reached")
			}
			if p.backend == "sandbox-exec" {
				profile := p.seatbeltProfile()
				for _, r := range reach {
					if strings.Contains(profile, "(allow file-write* (subpath "+quote(r)+"))") {
						t.Errorf("the profile lets commands write %s", r)
					}
				}
				return
			}
			// Under bubblewrap the temp folder is the sandbox's own, so a write
			// that lands is seen: none may.
			gd := filepath.Join(repo, ".git")
			if _, err := runIn(t, s, ws, "touch "+filepath.Join(gd, "planted")); err == nil {
				t.Error("a command wrote the git folder outside the workspace")
			}
			if _, err := os.Stat(filepath.Join(gd, "planted")); err == nil {
				t.Error("the git folder outside the workspace was written")
			}
		})
	}
}
