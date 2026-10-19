package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func gitAt(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
}

// The workbench lists a subagent's worktree by hunk, and a take puts the
// accepted hunks in as the person's writes, recorded, and removes it.
func TestWorkbenchTakesAWorktreeByHunk(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	wb := manualBench(t, nil)
	lines := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	wb.write("a.txt", lines)
	gitAt(t, wb.workspace, "init", "-q", "-b", "main")
	gitAt(t, wb.workspace, "add", ".")
	gitAt(t, wb.workspace, "commit", "-q", "-m", "init")
	gitAt(t, wb.workspace, "worktree", "add", "-q", "-b", "abhed/k3f9q2", ".abhed-worktrees/k3f9q2", "HEAD")
	wt := filepath.Join(wb.workspace, ".abhed-worktrees", "k3f9q2")
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte(strings.Replace(strings.Replace(lines, "two", "TWO", 1), "nine", "NINE", 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	var list worktreesResponse
	if rec := wb.get("acme", "worktrees"); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if len(list.Worktrees) != 1 || list.Worktrees[0].ID != "k3f9q2" || len(list.Worktrees[0].Files) != 1 || len(list.Worktrees[0].Files[0].Hunks) != 2 {
		t.Fatalf("list %+v", list)
	}
	rec := wb.send("acme", "POST", "worktrees/k3f9q2/take", takeRequest{Accept: map[string][]int{"a.txt": {0}}, Remove: true})
	var res agent.TakeResult
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &res) != nil {
		t.Fatalf("take: %d %s", rec.Code, rec.Body)
	}
	if len(res.Taken) != 1 || res.Left != 1 || !res.Removed {
		t.Fatalf("result %+v", res)
	}
	got, _ := os.ReadFile(filepath.Join(wb.workspace, "a.txt"))
	if !strings.Contains(string(got), "TWO") || strings.Contains(string(got), "NINE") {
		t.Fatalf("a.txt:\n%s", got)
	}
	taken := false
	for _, e := range wb.events() {
		taken = taken || e.Type == agent.EvWorktreeTaken
	}
	if !taken {
		t.Fatal("no worktree.taken in the record")
	}
	if rec := wb.send("acme", "POST", "worktrees/nope/take", takeRequest{}); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown worktree: %d", rec.Code)
	}
}
