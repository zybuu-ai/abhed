package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// /take lists a subagent's worktree, asks hunk by hunk, and puts what was
// accepted into the workspace as the person's write, recorded.
func TestTakeAWorktreeFromTheCLI(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	g := newSessRig(t)
	git := func(dir string, args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	lines := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	_ = os.WriteFile(filepath.Join(g.ws, "a.txt"), []byte(lines), 0o644)
	_ = os.WriteFile(filepath.Join(g.ws, ".gitignore"), []byte(".abhed/\n"), 0o644)
	git(g.ws, "init", "-q", "-b", "main")
	git(g.ws, "add", ".")
	git(g.ws, "commit", "-q", "-m", "init")
	git(g.ws, "worktree", "add", "-q", "-b", "abhed/k3f9q2", ".abhed-worktrees/k3f9q2", "HEAD")
	wt := filepath.Join(g.ws, ".abhed-worktrees", "k3f9q2", "a.txt")
	_ = os.WriteFile(wt, []byte(strings.Replace(strings.Replace(lines, "two", "TWO", 1), "nine", "NINE", 1)), 0o644)

	c := g.start("-mode", "accept-edits")
	g.ask(c, "hello")
	c.command("/take", "k3f9q2")
	c.command("/take k3f9q2", "Take change 1 of 2")
	c.command("1", "Take change 2 of 2") // take the first
	c.command("2", "Put 1 of 2")         // skip the second
	c.command("1", "took a.txt")         // take it, keep the worktree
	exit(c)
	got, _ := os.ReadFile(filepath.Join(g.ws, "a.txt"))
	if !strings.Contains(string(got), "TWO") || strings.Contains(string(got), "NINE") {
		t.Fatalf("a.txt:\n%s", got)
	}
	evs := verified(t, g.record(), g.sessions()[0].ID)
	if count(evs, agent.EvWorktreeTaken) != 1 || count(evs, agent.EvCheckpoint) != 1 {
		t.Fatalf("%d worktree.taken, %d checkpoint.saved", count(evs, agent.EvWorktreeTaken), count(evs, agent.EvCheckpoint))
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatal("the worktree was removed")
	}
}
