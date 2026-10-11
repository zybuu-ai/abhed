package hostgit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// seen is a sandbox that runs each line on the host, noting it.
type seen struct {
	tier  sandbox.Tier
	mu    sync.Mutex
	lines []string
}

func (s *seen) Tier() sandbox.Tier        { return s.tier }
func (s *seen) Available() (bool, string) { return true, "" }
func (s *seen) Describe() string          { return "seen" }
func (s *seen) Command(ctx context.Context, cwd, line string) *exec.Cmd {
	s.mu.Lock()
	s.lines = append(s.lines, line)
	s.mu.Unlock()
	cmd := exec.CommandContext(ctx, "sh", "-c", line)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GIT_DIR=/nonexistent")
	return cmd
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", "-b", "main", ws).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return ws
}

// With a sandbox registered for the workspace, Abhed's own git runs inside
// it, with git's variables the sandbox passes on unset and the safety list
// still set; worktree add stays on the host, and a container tier leaves all of it there.
func TestRegisteredSandboxRunsGit(t *testing.T) {
	ws := gitRepo(t)
	sb := &seen{tier: sandbox.TierProcess}
	defer Register(ws, sb)()
	if !Confined(filepath.Join(ws, "sub")) || Confined(t.TempDir()) {
		t.Fatal("confinement does not follow the workspace")
	}
	out, err := New(context.Background(), ws).Command(context.Background(), "rev-parse", "--git-dir").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse inside the sandbox: %v %s", err, out)
	}
	sb.mu.Lock()
	lines := strings.Join(sb.lines, "\n")
	sb.mu.Unlock()
	for _, want := range []string{"rev-parse", "--absolute-git-dir", "GIT_CONFIG_KEY_0", "core.fsmonitor", "GIT_ALLOW_PROTOCOL"} {
		if !strings.Contains(lines, want) {
			t.Errorf("no %q in the sandbox's lines:\n%s", want, lines)
		}
	}
	if cmd := New(context.Background(), ws).Command(context.Background(), "worktree", "add", "x"); len(cmd.Args) == 0 || filepath.Base(cmd.Args[0]) != "git" {
		t.Errorf("worktree add went to the sandbox: %v", cmd.Args)
	}
	if cmd := New(context.Background(), ws).Command(context.Background(), "worktree", "list"); filepath.Base(cmd.Path) == "git" {
		t.Errorf("worktree list stayed on the host: %v", cmd.Args)
	}

	c := &seen{tier: sandbox.TierContainer}
	defer Register(ws, c)()
	if Confined(ws) {
		t.Fatal("a container tier confines the host's git")
	}
}

// The process sandbox runs Abhed's git and gives the same answer as the host.
func TestProcessSandboxRunsGit(t *testing.T) {
	ws := gitRepo(t)
	t.Setenv("HOME", t.TempDir())
	p := sandbox.DefaultPolicy(ws)
	sb := sandbox.NewProcess(p)
	if ok, why := sb.Available(); !ok {
		t.Skip(why)
	}
	defer Register(ws, sb)()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := New(context.Background(), ws).Command(context.Background(), "status", "--porcelain").Output()
	if err != nil || !strings.Contains(string(out), "a.txt") {
		t.Fatalf("status inside the process sandbox: %v %q", err, out)
	}
}

// Abhed's worktree round trip works with the process sandbox registered:
// add on the host, then a commit, its status and the branch's removal inside it.
func TestProcessSandboxWorktreeRoundTrip(t *testing.T) {
	ws := gitRepo(t)
	t.Setenv("HOME", t.TempDir())
	ident := []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}
	if out, err := exec.Command("sh", "-c", "cd "+ws+" && git config user.name t && git config user.email t@t && git commit -q --allow-empty -m one").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	// Git's files held, as every surface now runs commands.
	p := sandbox.DefaultPolicy(ws)
	p.ProtectGit = true
	sb := sandbox.NewProcess(p)
	if ok, why := sb.Available(); !ok {
		t.Skip(why)
	}
	defer Register(ws, sb)()
	ctx := context.Background()
	r := New(ctx, ws)
	parent, err := r.Worktrees(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "w1")
	if out, err := r.Command(ctx, "worktree", "add", "-q", "-b", "abhed/w1", dir, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v %s", err, out)
	}
	if err := r.Placed(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := New(ctx, dir)
	if out, err := w.CommandWith(ctx, nil, ident, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("add in the sandbox: %v %s", err, out)
	}
	if out, err := w.Command(ctx, "commit", "-q", "-m", "two").CombinedOutput(); err != nil {
		t.Fatalf("commit in the sandbox: %v %s", err, out)
	}
	if ok, err := Untouched(ctx, dir, "HEAD~1"); err != nil || ok {
		t.Fatalf("untouched after a commit: %v %v", ok, err)
	}
	if out, err := r.Command(ctx, "worktree", "remove", "--force", dir).CombinedOutput(); err != nil {
		t.Fatalf("worktree remove: %v %s", err, out)
	}
	if out, err := r.Command(ctx, "branch", "-D", "abhed/w1").CombinedOutput(); err != nil {
		t.Fatalf("branch -D in the sandbox: %v %s", err, out)
	}
}
