package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "init")
	return dir
}

// Isolated tasks each get their own worktree on their own branch; what they
// write lands there and nowhere else, and the parent is told how to merge.
func TestParallelTasksInWorktreesDoNotTouchEachOtherOrMain(t *testing.T) {
	ws := gitRepo(t)
	var seen sync.Map
	spawn := func(ctx context.Context, req SubagentRequest) (string, error) {
		if req.Workspace == "" {
			return "", os.ErrInvalid
		}
		seen.Store(req.Description, req.Workspace)
		// Each child writes a file named after itself into ITS workspace.
		return "wrote", os.WriteFile(filepath.Join(req.Workspace, req.Description+".txt"), []byte("x"), 0o644)
	}
	tool := Tasks{Spawn: spawn, Workspace: ws}
	args, _ := json.Marshal(map[string]any{
		"isolation": "worktree",
		"tasks": []map[string]any{
			{"prompt": "a", "description": "alpha"},
			{"prompt": "b", "description": "beta"},
		},
	})
	res := tool.Run(context.Background(), nil, args)
	if res.IsError {
		t.Fatalf("tasks failed: %s", res.Content)
	}

	// Two distinct worktrees, both under the workspace.
	wa, _ := seen.Load("alpha")
	wb, _ := seen.Load("beta")
	if wa == nil || wb == nil || wa == wb {
		t.Fatalf("worktrees: alpha=%v beta=%v", wa, wb)
	}
	for _, w := range []string{wa.(string), wb.(string)} {
		if !strings.HasPrefix(w, filepath.Join(ws, WorktreeDir)) {
			t.Errorf("worktree %s is outside %s", w, WorktreeDir)
		}
	}
	// alpha's file is in alpha's tree only; the main tree has neither.
	if _, err := os.Stat(filepath.Join(wa.(string), "alpha.txt")); err != nil {
		t.Error("alpha's file missing from alpha's worktree")
	}
	if _, err := os.Stat(filepath.Join(wb.(string), "alpha.txt")); err == nil {
		t.Error("alpha's file leaked into beta's worktree")
	}
	if _, err := os.Stat(filepath.Join(ws, "alpha.txt")); err == nil {
		t.Error("alpha's file leaked into the main tree")
	}
	// The report says what changed and how to take it.
	for _, want := range []string{"## Task 1 — alpha", "## Task 2 — beta", "branch abhed/", "UNCOMMITTED", "git merge abhed/"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("report missing %q:\n%s", want, res.Content)
		}
	}
	// The worktree directory is excluded from the main tree's status.
	out, _ := exec.Command("git", "-C", ws, "status", "--porcelain").Output()
	if strings.Contains(string(out), WorktreeDir) {
		t.Errorf("worktrees show up in git status of the main tree:\n%s", out)
	}
}

// Without isolation the children share the parent's workspace, run at the
// same time, and are bounded by MaxParallel.
func TestParallelTasksRunConcurrentlyUpToTheLimit(t *testing.T) {
	var inFlight, peak atomic.Int32
	spawn := func(ctx context.Context, req SubagentRequest) (string, error) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		return "done " + req.Description, nil
	}
	tool := Tasks{Spawn: spawn, Workspace: t.TempDir(), MaxParallel: 2}
	args, _ := json.Marshal(map[string]any{"tasks": []map[string]any{
		{"prompt": "1", "description": "one"}, {"prompt": "2", "description": "two"},
		{"prompt": "3", "description": "three"}, {"prompt": "4", "description": "four"},
	}})
	start := time.Now()
	res := tool.Run(context.Background(), nil, args)
	if res.IsError {
		t.Fatal(res.Content)
	}
	if peak.Load() != 2 {
		t.Errorf("peak concurrency %d, want exactly the limit of 2", peak.Load())
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Error("four 30ms tasks at concurrency 2 took too long — they ran serially")
	}
	if !strings.Contains(res.Content, "done four") {
		t.Errorf("report lost a task:\n%s", res.Content)
	}
}

// Asking for isolation outside a git repository is refused with a reason,
// before any subagent has spent a token.
func TestWorktreeIsolationNeedsAGitRepo(t *testing.T) {
	spawned := false
	tool := Tasks{Spawn: func(context.Context, SubagentRequest) (string, error) { spawned = true; return "", nil },
		Workspace: t.TempDir()}
	args, _ := json.Marshal(map[string]any{"isolation": "worktree",
		"tasks": []map[string]any{{"prompt": "x", "description": "x"}}})
	res := tool.Run(context.Background(), nil, args)
	if !res.IsError || !strings.Contains(res.Content, "git repository") {
		t.Errorf("expected a refusal naming git, got: %s", res.Content)
	}
	if spawned {
		t.Error("a subagent was spawned despite the refusal")
	}
}

// One failing task does not fail the others; the report says which failed.
func TestParallelTasksReportPartialFailure(t *testing.T) {
	tool := Tasks{Spawn: func(ctx context.Context, req SubagentRequest) (string, error) {
		if req.Description == "bad" {
			return "", context.DeadlineExceeded
		}
		return "fine", nil
	}, Workspace: t.TempDir()}
	args, _ := json.Marshal(map[string]any{"tasks": []map[string]any{
		{"prompt": "a", "description": "good"}, {"prompt": "b", "description": "bad"},
	}})
	res := tool.Run(context.Background(), nil, args)
	if res.IsError {
		t.Error("a partial failure was reported as total failure")
	}
	if !strings.Contains(res.Content, "FAILED") || !strings.Contains(res.Content, "[1 of 2 tasks failed]") {
		t.Errorf("report does not name the failure:\n%s", res.Content)
	}
}

// workdirAdapter plays turns planned from the working directory the system
// prompt names, which for an isolated subagent is its worktree.
type workdirAdapter struct {
	scriptedAdapter
	plan func(dir string) []scriptedTurn
}

func (w *workdirAdapter) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	if w.turns == nil {
		_, after, _ := strings.Cut(req.System, "Working directory: ")
		dir, _, _ := strings.Cut(after, "\n")
		w.turns = w.plan(dir)
	}
	return w.scriptedAdapter.Complete(ctx, req)
}

// An isolated subagent really works in its worktree: its file tools and its
// sandboxed commands write there, and nothing lands in the main tree.
func TestIsolatedSubagentWritesInItsWorktree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := gitRepo(t)
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(tools.Read{}, tools.Write{})
	sandboxed := false
	// Where the sandbox cannot run a command here (no network namespace on a
	// hosted runner), only the file tools are tried.
	if sb := sandbox.NewProcess(sandbox.DefaultPolicy(ws)); sb.Command(context.Background(), ws, "true").Run() == nil {
		reg = tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Bash{Sandbox: sb.Command})
		sandboxed = true
	}
	var worktree string
	adapter := &workdirAdapter{plan: func(dir string) []scriptedTurn {
		worktree = dir
		turns := []scriptedTurn{{calls: []model.ToolCall{call("write", map[string]string{
			"path": filepath.Join(dir, "MARKER.txt"), "content": "from the subagent\n"})}}}
		if sandboxed {
			turns = append(turns, scriptedTurn{calls: []model.ToolCall{call("bash", map[string]string{
				"command": "echo ran > BASH.txt", "description": "write a file"})}})
		}
		return append(turns, scriptedTurn{text: "Wrote the marker."})
	}}
	sess, err := tools.NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	f := &SubagentFactory{Adapter: adapter, Tools: reg, Policy: policy.New(policy.ModeDefault),
		Approver: AutoApprove{Yes: true}, Session: sess, Store: NewMemStore(),
		Budget: NewBudget(1_000_000, 10, false), Config: DefaultConfig(), Workspace: ws}
	args, _ := json.Marshal(map[string]any{"isolation": "worktree",
		"tasks": []map[string]any{{"prompt": "write the marker", "description": "marker"}}})
	res := Tasks{Spawn: f.Spawn, Workspace: ws}.Run(context.Background(), nil, args)
	if res.IsError || !strings.Contains(res.Content, "UNCOMMITTED") {
		t.Fatalf("tasks: %s", res.Content)
	}
	if !strings.HasPrefix(worktree, filepath.Join(ws, WorktreeDir)+string(filepath.Separator)) {
		t.Fatalf("the subagent worked in %q", worktree)
	}
	if got, _ := os.ReadFile(filepath.Join(worktree, "MARKER.txt")); string(got) != "from the subagent\n" {
		t.Fatalf("the subagent's write did not land in its worktree: %q\n%s", got, res.Content)
	}
	if got, _ := os.ReadFile(filepath.Join(worktree, "BASH.txt")); sandboxed && string(got) != "ran\n" {
		t.Fatalf("the subagent's command did not run in its worktree: %q\n%s", got, res.Content)
	}
	for _, f := range []string{"MARKER.txt", "BASH.txt"} {
		if _, err := os.Stat(filepath.Join(ws, f)); err == nil {
			t.Errorf("%s leaked into the main tree", f)
		}
	}
}

// A worktree the subagent left unchanged is removed, with its branch.
func TestUnchangedWorktreeIsRemoved(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := gitRepo(t)
	spawn := func(context.Context, SubagentRequest) (string, error) { return "nothing to do", nil }
	args, _ := json.Marshal(map[string]any{"isolation": "worktree",
		"tasks": []map[string]any{{"prompt": "look", "description": "look"}}})
	res := Tasks{Spawn: spawn, Workspace: ws}.Run(context.Background(), nil, args)
	if !strings.Contains(res.Content, "no changes; removed") {
		t.Fatalf("report: %s", res.Content)
	}
	if _, err := os.Lstat(filepath.Join(ws, WorktreeDir)); err == nil {
		t.Error("the empty worktree was left behind")
	}
	if out, _ := exec.Command("git", "-C", ws, "branch", "--list", "abhed/*").Output(); len(out) != 0 {
		t.Errorf("the branch was left behind: %s", out)
	}
}

// A worktree is removed only when it is exactly as it was made. One whose
// subagent committed, left only ignored files, or whose state cannot be read
// is kept with its branch: any of them may hold the work.
func TestWorktreeHoldingWorkIsKept(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for name, c := range map[string]struct {
		work func(t *testing.T, dir string)
		want string
	}{
		"committed": {func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("done\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"add", "work.txt"}, {"commit", "-q", "-m", "work"}} {
				cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
				cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %s", args, out)
				}
			}
		}, "holds commits or ignored files"},
		"only ignored files": {func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "report.log"), []byte("findings\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "holds commits or ignored files"},
		"state unreadable": {func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("not a gitdir\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "could not be read"},
	} {
		t.Run(name, func(t *testing.T) {
			ws := gitRepo(t)
			if err := os.MkdirAll(filepath.Join(ws, ".git", "info"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws, ".git", "info", "exclude"), []byte("*.log\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var dir string
			spawn := func(_ context.Context, req SubagentRequest) (string, error) {
				dir = req.Workspace
				c.work(t, dir)
				return "worked", nil
			}
			args, _ := json.Marshal(map[string]any{"isolation": "worktree",
				"tasks": []map[string]any{{"prompt": "work", "description": "work"}}})
			res := Tasks{Spawn: spawn, Workspace: ws}.Run(context.Background(), nil, args)
			if !strings.Contains(res.Content, c.want) || strings.Contains(res.Content, "removed") {
				t.Fatalf("report: %s", res.Content)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("the worktree was removed: %v", err)
			}
			if out, _ := exec.Command("git", "-C", ws, "branch", "--list", "abhed/*").Output(); len(out) == 0 {
				t.Fatal("the branch was deleted")
			}
		})
	}
}
