package forge

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Work is one resolution: the issue, the branch it is worked on, and where.
type Work struct {
	Issue  Issue
	Branch string
	Dir    string // the worktree
	Base   string
}

// Runner does the actual work in the worktree. It is the agent session; the
// resolver knows nothing about models.
type Runner func(ctx context.Context, dir, prompt string) error

// Prompt is what the agent is asked, built from the issue.
func Prompt(is Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Resolve issue #%d: %s\n\n", is.Ref.Number, is.Title)
	if strings.TrimSpace(is.Body) != "" {
		b.WriteString(is.Body)
		b.WriteString("\n\n")
	}
	b.WriteString("Make the change in this checkout, add or update tests, and run the tests. " +
		"Do not commit and do not push: that is done for you once you are finished. " +
		"If the issue cannot be resolved, say why instead of guessing.")
	return b.String()
}

// Begin checks the repository out on a fresh branch in its own worktree, so
// the user's checkout is untouched while the agent works.
func Begin(ctx context.Context, repo string, is Issue, base string) (*Work, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("git is not installed")
	}
	if _, err := git(ctx, repo, "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("%s is not a git repository", repo)
	}
	branch := fmt.Sprintf("abhed/issue-%d", is.Ref.Number)
	dir := filepath.Join(repo, ".abhed", "worktrees", fmt.Sprintf("issue-%d", is.Ref.Number))
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return nil, err
	}
	start := "HEAD"
	if base != "" {
		if _, err := git(ctx, repo, "rev-parse", "--verify", "origin/"+base); err == nil {
			start = "origin/" + base
		} else if _, err := git(ctx, repo, "rev-parse", "--verify", base); err == nil {
			start = base
		}
	}
	// A branch left by an earlier attempt is replaced, not built on.
	_, _ = git(ctx, repo, "worktree", "remove", "--force", dir)
	_, _ = git(ctx, repo, "branch", "-D", branch)
	if _, err := git(ctx, repo, "worktree", "add", "-b", branch, dir, start); err != nil {
		return nil, err
	}
	return &Work{Issue: is, Branch: branch, Dir: dir, Base: base}, nil
}

// Commit records what the agent changed, as one commit that names the issue.
// The author is the repository's configured one: the person who ran this.
func (w *Work) Commit(ctx context.Context) (string, error) {
	if _, err := git(ctx, w.Dir, "add", "-A"); err != nil {
		return "", err
	}
	status, err := git(ctx, w.Dir, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) == "" {
		return "", ErrNoChange
	}
	msg := fmt.Sprintf("Resolve #%d: %s\n\nSee %s", w.Issue.Ref.Number, w.Issue.Title, w.Issue.URL)
	if _, err := git(ctx, w.Dir, "commit", "-q", "-m", msg); err != nil {
		return "", err
	}
	return git(ctx, w.Dir, "rev-parse", "--short", "HEAD")
}

// Push sends the branch to the remote over HTTPS with the forge's token,
// handed to git through its environment so it appears on no command line.
func (w *Work) Push(ctx context.Context, remote string, auth string) error {
	env := append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: "+auth,
		"GIT_TERMINAL_PROMPT=0",
	)
	cmd := exec.CommandContext(ctx, "git", "-C", w.Dir, "push", "-u", remote, w.Branch+":"+w.Branch)
	cmd.Env = env
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("git push: %s", msg)
	}
	return nil
}

// Diff summarises the change for the pull request body.
func (w *Work) Diff(ctx context.Context) string {
	out, err := git(ctx, w.Dir, "diff", "--stat", "HEAD~1")
	if err != nil {
		return ""
	}
	return out
}

// Cleanup removes the worktree; the branch stays, since it was pushed.
func (w *Work) Cleanup(ctx context.Context, repo string) {
	_, _ = git(ctx, repo, "worktree", "remove", "--force", w.Dir)
}

// Remote reports the URL of the named remote, for the push and for checking
// it is the issue's repository.
func Remote(ctx context.Context, repo, name string) (string, error) {
	return git(ctx, repo, "remote", "get-url", name)
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}
