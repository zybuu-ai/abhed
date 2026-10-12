package hostgit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// Only a whole, plain worktree or submodule command is brokered.
func TestBrokeredLines(t *testing.T) {
	for line, want := range map[string]bool{
		"git worktree add -b x wt HEAD":        true,
		"git worktree remove --force wt":       true,
		"git worktree prune":                   true,
		"git submodule update --init":          true,
		"git worktree add wt; rm -rf /":        false,
		"git worktree add $(evil)":             false,
		"git worktree add --reason=x wt":       false,
		"git status":                           false,
		"git -c core.fsmonitor=x worktree add": false,
		"git submodule add https://x/y.git y":  false,
	} {
		if _, got := brokered(line); got != want {
			t.Errorf("%q: brokered %v", line, got)
		}
	}
}

// Under the process tier, git worktree add and remove run on the host and
// work; a path outside the workspace is refused; other lines go to the sandbox.
func TestBrokerRunsWorktreesOnTheHost(t *testing.T) {
	ws := gitRepo(t)
	t.Setenv("HOME", t.TempDir())
	if out, err := exec.Command("sh", "-c", "cd "+ws+" && git -c user.name=t -c user.email=t@t commit -q --allow-empty -m one").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	var seen []string
	inner := func(ctx context.Context, cwd, line string) *exec.Cmd {
		seen = append(seen, line)
		return exec.CommandContext(ctx, "true")
	}
	process := func() sandbox.Tier { return sandbox.TierProcess }
	run := Broker(ws, process, inner)
	ctx := context.Background()
	if out, err := run(ctx, ws, "git worktree add -q -b w1 wt HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(ws, "wt", ".git")); err != nil {
		t.Fatalf("no worktree: %v", err)
	}
	if out, err := run(ctx, ws, "git worktree remove --force wt").CombinedOutput(); err != nil {
		t.Fatalf("worktree remove: %v %s", err, out)
	}
	if err := run(ctx, ws, "git worktree add "+t.TempDir()+"/out").Run(); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("a worktree outside the workspace: %v", err)
	}
	_ = run(ctx, ws, "git status").Run()
	if len(seen) != 1 || seen[0] != "git status" {
		t.Fatalf("the sandbox saw %v", seen)
	}
	// The container tier is left to its sandbox.
	container := func() sandbox.Tier { return sandbox.TierContainer }
	_ = Broker(ws, container, inner)(ctx, ws, "git worktree prune").Run()
	if len(seen) != 2 {
		t.Fatalf("a container tier's line was brokered: %v", seen)
	}
}
