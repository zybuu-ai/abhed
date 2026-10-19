package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// §7.4.1: Studio lists a subagent's worktree by hunk and takes the accepted
// ones as the person's writes; the worktree goes only when asked.
func TestStudioTakesAWorktreeByHunk(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r := newStudioRig(t, "")
	git := func(dir string, args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	lines := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	r.write("a.txt", lines)
	r.write(".gitignore", ".abhed/\n")
	git(r.ws, "init", "-q", "-b", "main")
	git(r.ws, "add", ".")
	git(r.ws, "commit", "-q", "-m", "init")
	git(r.ws, "worktree", "add", "-q", "-b", "abhed/k3f9q2", ".abhed-worktrees/k3f9q2", "HEAD")
	wt := filepath.Join(r.ws, ".abhed-worktrees", "k3f9q2")
	_ = os.WriteFile(filepath.Join(wt, "a.txt"), []byte(strings.Replace(strings.Replace(lines, "two", "TWO", 1), "nine", "NINE", 1)), 0o644)

	if !slices.Contains(r.cl.conn.acpFeatures(), "worktrees") {
		t.Fatalf("features: %v", r.cl.conn.acpFeatures())
	}
	id := r.open()
	var list struct {
		Worktrees []agent.WorktreeChanges `json:"worktrees"`
	}
	r.cl.ok("_abhed/worktrees/list", map[string]any{"sessionId": id}, &list)
	if len(list.Worktrees) != 1 || list.Worktrees[0].ID != "k3f9q2" || len(list.Worktrees[0].Files) != 1 || len(list.Worktrees[0].Files[0].Hunks) != 2 {
		t.Fatalf("list %+v", list.Worktrees)
	}
	var res agent.TakeResult
	r.cl.ok("_abhed/worktrees/take", map[string]any{"sessionId": id, "worktree": "k3f9q2", "accept": map[string][]int{"a.txt": {1}}}, &res)
	if len(res.Taken) != 1 || res.Left != 1 || res.Removed {
		t.Fatalf("take %+v", res)
	}
	if got := r.read("a.txt"); !strings.Contains(got, "NINE") || strings.Contains(got, "TWO") {
		t.Fatalf("a.txt:\n%s", got)
	}
	if taken, actors := r.recorded(id, agent.EvWorktreeTaken); len(taken) != 1 || actors[0] != agent.ActorUser {
		t.Fatalf("worktree.taken: %v", taken)
	}
	// Taking the same hunk again changes nothing; remove takes the worktree away.
	r.cl.ok("_abhed/worktrees/take", map[string]any{"sessionId": id, "worktree": "k3f9q2", "accept": map[string][]int{"a.txt": {1}}, "remove": true}, &res)
	if !res.Removed || len(res.Conflicts) != 0 {
		t.Fatalf("second take %+v", res)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("the worktree is still there")
	}
	r.cl.refused(errParams, "_abhed/worktrees/take", map[string]any{"sessionId": id, "worktree": "k3f9q2"})
	r.cl.refused(errParams, "_abhed/worktrees/take", map[string]any{"sessionId": id})
}
