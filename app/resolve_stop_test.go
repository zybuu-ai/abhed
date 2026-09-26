//go:build unix

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/zybuu-ai/abhed/internal/forge"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// stubResolveRun makes the run itself do what run says, in place of a model.
func stubResolveRun(t *testing.T, fg *fakeForge, run func(ctx context.Context, dir string) error) {
	t.Helper()
	oldForge, oldRunner := newForge, newResolveRunner
	newForge = func(context.Context, forge.Ref, forge.Options) (forge.Forge, error) { return fg, nil }
	newResolveRunner = func(context.Context, abhed.Options) (forge.Runner, error) {
		return func(ctx context.Context, dir, _ string) error { return run(ctx, dir) }, nil
	}
	t.Cleanup(func() { newForge, newResolveRunner = oldForge, oldRunner })
}

// A run that fails, or a resolve that is stopped, leaves the worktree it says
// it keeps; nothing is pushed.
func TestResolveKeepsTheWorktreeOfAFailedOrStoppedRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(ctx context.Context, dir string) error
		code int
	}{
		{"failed", func(context.Context, string) error { return errors.New("the model endpoint went away") }, 1},
		{"commit failed", func(_ context.Context, dir string) error {
			// A change holding a repository of its own, which is not committed.
			return os.MkdirAll(filepath.Join(dir, "sub", ".git"), 0o755)
		}, 1},
		{"stopped", func(ctx context.Context, _ string) error {
			_ = syscall.Kill(os.Getpid(), syscall.SIGINT) // this test process, whose resolve is catching it
			<-ctx.Done()
			return ctx.Err()
		}, 128 + int(syscall.SIGINT)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, remote := resolveRepo(t)
			fg := &fakeForge{}
			stubResolveRun(t, fg, tc.run)
			if code := resolveCmd(repo, []string{"-y", "https://git.example/t/r/issues/5"}); code != tc.code {
				t.Fatalf("exit %d, want %d", code, tc.code)
			}
			if _, err := os.Stat(filepath.Join(repo, ".abhed", "worktrees", "issue-5")); err != nil {
				t.Fatalf("the worktree was not kept: %v", err)
			}
			if fg.opened != nil || branchOnRemote(t, remote) {
				t.Fatal("a failed or stopped run was pushed")
			}
		})
	}
}
