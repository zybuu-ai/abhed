package app

import (
	"os"
	"path/filepath"
	"testing"
)

// The branch is read from HEAD, in a repository, a worktree (whose .git is
// a file naming the git directory) and a subfolder of either.
func TestGitBranch(t *testing.T) {
	repo := t.TempDir()
	must(os.MkdirAll(filepath.Join(repo, ".git"), 0o700))
	must(os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))
	sub := filepath.Join(repo, "a", "b")
	must(os.MkdirAll(sub, 0o700))
	if got := gitBranch(sub); got != "main" {
		t.Errorf("repository: %q", got)
	}

	wt := t.TempDir()
	gitdir := filepath.Join(repo, ".git", "worktrees", "wt")
	must(os.MkdirAll(gitdir, 0o700))
	must(os.WriteFile(filepath.Join(gitdir, "HEAD"), []byte("ref: refs/heads/feature/x\n"), 0o600))
	must(os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o600))
	if got := gitBranch(wt); got != "feature/x" {
		t.Errorf("worktree: %q", got)
	}

	must(os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("d5f98771234567890\n"), 0o600))
	if got := gitBranch(repo); got != "d5f9877" {
		t.Errorf("detached: %q", got)
	}
	if got := gitBranch(t.TempDir()); got != "" {
		t.Errorf("no repository: %q", got)
	}
}
