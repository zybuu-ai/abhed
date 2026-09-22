package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/forge"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

type fakeForge struct {
	opened *forge.PullRequest
}

func (f *fakeForge) Kind() string     { return forge.KindGitea }
func (f *fakeForge) PushAuth() string { return "Basic eDp5" }
func (f *fakeForge) Issue(context.Context, forge.Ref) (forge.Issue, error) {
	return forge.Issue{Ref: forge.Ref{Number: 5}, Title: "Add two", Body: "a.txt needs a second line", URL: "https://git.example/t/r/issues/5"}, nil
}
func (f *fakeForge) DefaultBranch(context.Context, forge.Ref) (string, error) { return "main", nil }
func (f *fakeForge) OpenPullRequest(_ context.Context, _ forge.Ref, pr forge.PullRequest) (string, error) {
	f.opened = &pr
	return "https://git.example/t/r/pulls/9", nil
}

func resolveRepo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	sh := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	sh("", "init", "-q", "--bare", remote)
	repo := filepath.Join(t.TempDir(), "repo")
	sh("", "clone", "-q", remote, repo)
	sh(repo, "config", "user.email", "t@example.com")
	sh(repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sh(repo, "add", "a.txt")
	sh(repo, "commit", "-q", "-m", "init")
	sh(repo, "push", "-q", "origin", "HEAD")
	return repo, remote
}

func stubResolve(t *testing.T, fg *fakeForge, change string) {
	t.Helper()
	oldForge, oldRunner := newForge, newResolveRunner
	newForge = func(context.Context, forge.Ref, forge.Options) (forge.Forge, error) { return fg, nil }
	newResolveRunner = func(_ context.Context, opts abhed.Options) (forge.Runner, error) {
		return func(_ context.Context, dir, prompt string) error {
			if !strings.Contains(prompt, "Add two") || !strings.Contains(prompt, "Do not commit") {
				t.Errorf("prompt: %q", prompt)
			}
			if opts.Workspace != dir {
				t.Errorf("the run's workspace is %s, not the worktree %s", opts.Workspace, dir)
			}
			if change == "" {
				return nil
			}
			return os.WriteFile(filepath.Join(dir, "a.txt"), []byte(change), 0o600)
		}, nil
	}
	t.Cleanup(func() { newForge, newResolveRunner = oldForge, oldRunner })
}

func branchOnRemote(t *testing.T, remote string) bool {
	t.Helper()
	out, _ := exec.Command("git", "--git-dir", remote, "branch", "--list", "abhed/issue-5").CombinedOutput()
	return strings.Contains(string(out), "abhed/issue-5")
}

func TestResolveOpensAPullRequestFromAWorktree(t *testing.T) {
	repo, remote := resolveRepo(t)
	fg := &fakeForge{}
	stubResolve(t, fg, "one\ntwo\n")

	if code := resolveCmd(repo, []string{"-y", "https://git.example/t/r/issues/5"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if fg.opened == nil || fg.opened.Head != "abhed/issue-5" || fg.opened.Base != "main" || !strings.Contains(fg.opened.Title, "Resolve #5") {
		t.Fatalf("pull request: %+v", fg.opened)
	}
	if !branchOnRemote(t, remote) {
		t.Fatal("the branch was not pushed")
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(got) != "one\n" {
		t.Fatalf("the checkout was changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, ".abhed", "worktrees", "issue-5")); err == nil {
		t.Fatal("the worktree was left behind")
	}
}

// Without -y or an allow rule, and with nobody at a terminal, the request is
// not opened and nothing is pushed; a deny rule refuses it outright.
func TestResolveWillNotOpenAPullRequestUnasked(t *testing.T) {
	repo, remote := resolveRepo(t)
	fg := &fakeForge{}
	stubResolve(t, fg, "one\ntwo\n")

	if code := resolveCmd(repo, []string{"https://git.example/t/r/issues/5"}); code == 0 {
		t.Fatal("opened without approval")
	}
	if fg.opened != nil || branchOnRemote(t, remote) {
		t.Fatal("pushed or opened without approval")
	}

	if err := os.MkdirAll(filepath.Join(repo, ".abhed"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".abhed", "config.json"), []byte(`{"permissions":{"deny":["forge_pr(*)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := resolveCmd(repo, []string{"-y", "https://git.example/t/r/issues/5"}); code == 0 {
		t.Fatal("a deny rule was overridden by -y")
	}
	if fg.opened != nil || branchOnRemote(t, remote) {
		t.Fatal("pushed or opened against a deny rule")
	}
}

func TestResolveReportsNoChange(t *testing.T) {
	repo, remote := resolveRepo(t)
	fg := &fakeForge{}
	stubResolve(t, fg, "")
	if code := resolveCmd(repo, []string{"-y", "https://git.example/t/r/issues/5"}); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if fg.opened != nil || branchOnRemote(t, remote) {
		t.Fatal("an empty run was pushed")
	}
}
