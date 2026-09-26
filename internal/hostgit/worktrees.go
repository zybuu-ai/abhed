package hostgit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// Worktrees makes <repo>/.abhed-worktrees, excluded from the repository's status,
// and returns it. A link there, or a path out of the repository or into the state, is refused.
func (r *Repo) Worktrees(ctx context.Context) (string, error) {
	ws, err := filepath.Abs(r.Dir)
	if err != nil {
		return "", err
	}
	c, err := tools.NewStateSet(ws).Confine(tools.RealPath(ws), ws)
	if err != nil {
		return "", err
	}
	defer c.Close()
	dir := filepath.Join(ws, tools.WorktreesDir)
	if err := c.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("making %s: %w", dir, err)
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s must be a folder of the repository, not a link or a file; remove it", dir)
	}
	r.exclude(ctx, c)
	return dir, nil
}

// NewWorktreeDir refuses a worktree path that something already holds, such
// as a link planted where the next checkout would be made.
func NewWorktreeDir(dir string) error {
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%s already exists; remove it", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveWorktrees removes the worktrees folder once it is empty.
func RemoveWorktrees(repo string) {
	_ = os.Remove(filepath.Join(repo, tools.WorktreesDir))
}

// exclude adds the worktrees folder to the local info/exclude, written as the file
// tools write, since the agent can change the git directory; one outside the workspace is left alone.
func (r *Repo) exclude(ctx context.Context, c *tools.Confined) {
	out, err := r.Command(ctx, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return
	}
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(r.Dir, gitDir)
	}
	p := filepath.Join(gitDir, "info", "exclude")
	if !c.Contains(p) {
		return
	}
	existing, err := c.ReadFile(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return
	}
	line := "/" + tools.WorktreesDir + "/"
	for _, l := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(l) == line {
			return
		}
	}
	if err := c.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		existing = append(existing, '\n')
	}
	_ = c.WriteAtomic(p, append(existing, []byte("# Abhed's worktrees\n"+line+"\n")...), 0o644)
}

// Placed checks that a worktree git has just made is where it was asked to
// be, and not wherever a link swapped in meanwhile led; if not, it removes it.
func (r *Repo) Placed(ctx context.Context, dir string) error {
	ws, err := filepath.Abs(r.Dir)
	if err != nil {
		return err
	}
	want := filepath.Join(tools.RealPath(ws), tools.WorktreesDir, filepath.Base(dir))
	if got := tools.RealPath(dir); got != want {
		_ = r.Command(ctx, "worktree", "remove", "--force", dir).Run()
		return fmt.Errorf("the worktree was made at %s, not %s; a link was swapped in, so it was removed", got, want)
	}
	return nil
}

// Head returns the commit checked out in dir.
func Head(ctx context.Context, dir string) (string, error) {
	out, err := New(ctx, dir).Command(ctx, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("reading the worktree's commit: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Untouched reports whether the worktree at dir is as it was made from start:
// HEAD still at start, and nothing changed, untracked or ignored in it. Only
// such a worktree is removed with its branch; anything else may hold work.
func Untouched(ctx context.Context, dir, start string) (bool, error) {
	head, err := Head(ctx, dir)
	if err != nil {
		return false, err
	}
	out, err := New(ctx, dir).Command(ctx, "status", "--porcelain", "--ignored", "--untracked-files=all").Output()
	if err != nil {
		return false, fmt.Errorf("reading the worktree's status: %w", err)
	}
	return head == start && strings.TrimSpace(string(out)) == "", nil
}
