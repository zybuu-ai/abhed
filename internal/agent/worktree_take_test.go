package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// takeRig is a repository with one subagent worktree and a loop whose
// person writes through the write tool.
func takeRig(t *testing.T) (ws string, w Worktree, l *Loop, sess *tools.Session, store *MemStore) {
	t.Helper()
	ws = gitRepo(t)
	ctx := context.Background()
	wt, err := addWorktree(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	list, err := ListWorktrees(ctx, ws)
	if err != nil || len(list) != 1 || list[0].Branch != wt.Branch {
		t.Fatalf("list %+v %v", list, err)
	}
	sess, _ = tools.NewSession(ws)
	store = NewMemStore()
	l = &Loop{Tools: tools.NewRegistry(tools.Write{}), Policy: policy.New(policy.ModeDefault), Recorder: NewRecorder(store, "s-take", "")}
	return ws, list[0], l, sess, store
}

func putIn(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Accepted hunks reach the main tree, a rejected one does not, a new file is
// made, and the worktree is removed when asked; the record holds each write
// as the person's and the take itself.
func TestTakeWorktreeByHunk(t *testing.T) {
	ws, w, l, sess, store := takeRig(t)
	ctx := context.Background()
	lines := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	putIn(t, ws, "README.md", lines)
	commitAll(t, ws)
	// The worktree was made before that commit: give it the same base.
	gitIn(t, w.Dir, "merge", "-q", "--ff-only", "main")
	putIn(t, w.Dir, "README.md", strings.Replace(strings.Replace(lines, "two\n", "TWO\n", 1), "nine\n", "NINE\n", 1))
	putIn(t, w.Dir, "new.txt", "made\n")

	ch, err := w.Changes(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Files) != 2 || ch.Files[0].Path != "README.md" || len(ch.Files[0].Hunks) != 2 || ch.Files[1].Status != "added" {
		t.Fatalf("changes %+v", ch.Files)
	}
	res, err := l.Take(ctx, ws, w, map[string][]int{"README.md": {1}, "new.txt": {0}}, true, l.ManualTaker(sess))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Taken) != 2 || len(res.Conflicts) != 0 || res.Left != 1 || !res.Removed {
		t.Fatalf("result %+v", res)
	}
	got, _ := os.ReadFile(filepath.Join(ws, "README.md"))
	if !strings.Contains(string(got), "NINE") || strings.Contains(string(got), "TWO") {
		t.Fatalf("README.md:\n%s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "new.txt")); string(b) != "made\n" {
		t.Fatalf("new.txt %q", b)
	}
	if _, err := os.Stat(w.Dir); !os.IsNotExist(err) {
		t.Fatal("the worktree is still there")
	}
	if list, _ := ListWorktrees(ctx, ws); len(list) != 0 {
		t.Fatalf("still listed: %+v", list)
	}
	evs, _ := store.Events("s-take")
	writes, taken := 0, 0
	for _, e := range evs {
		var p struct {
			Tool string `json:"tool"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if e.Type == EvActionRequested && p.Tool == "write" && e.Actor == ActorUser {
			writes++
		}
		if e.Type == EvWorktreeTaken {
			taken++
		}
	}
	if writes != 2 || taken != 1 {
		t.Fatalf("%d writes, %d worktree.taken", writes, taken)
	}
}

// A hunk the main tree changed at meanwhile is a conflict and left alone; one
// elsewhere in the file still goes in, moved to where its lines now are.
func TestTakeWorktreeReportsConflicts(t *testing.T) {
	ws, w, l, sess, _ := takeRig(t)
	ctx := context.Background()
	lines := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	putIn(t, ws, "README.md", lines)
	commitAll(t, ws)
	gitIn(t, w.Dir, "merge", "-q", "--ff-only", "main")
	putIn(t, w.Dir, "README.md", strings.Replace(strings.Replace(lines, "two\n", "TWO\n", 1), "nine\n", "NINE\n", 1))
	// The main tree changes line two its own way, and adds a line at the top.
	putIn(t, ws, "README.md", "zero\n"+strings.Replace(lines, "two\n", "deux\n", 1))

	res, err := l.Take(ctx, ws, w, map[string][]int{"README.md": {0, 1}}, true, l.ManualTaker(sess))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 || res.Left != 1 || len(res.Taken) != 1 {
		t.Fatalf("result %+v", res)
	}
	got, _ := os.ReadFile(filepath.Join(ws, "README.md"))
	if want := "zero\none\ndeux\nthree\nfour\nfive\nsix\nseven\neight\nNINE\nten\n"; string(got) != want {
		t.Fatalf("README.md:\n%s", got)
	}
	// Removal was asked for, and done: the person chose to drop the rest.
	if !res.Removed {
		t.Fatal("not removed")
	}
}

// A deny rule holds for a take as for any write: the file is refused and
// left as it was, and a path in a .git folder is never taken.
func TestTakeWorktreeRespectsPolicy(t *testing.T) {
	ws, w, l, sess, _ := takeRig(t)
	ctx := context.Background()
	if err := l.Policy.AddDeny("write(" + filepath.Join(tools.RealPath(ws), "README.md") + ")"); err != nil {
		t.Fatal(err)
	}
	putIn(t, w.Dir, "README.md", "changed\n")
	res, err := l.Take(ctx, ws, w, map[string][]int{"README.md": {0}}, false, l.ManualTaker(sess))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "Denied") || len(res.Taken) != 0 || res.Removed {
		t.Fatalf("result %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "README.md")); string(b) != "hello\n" {
		t.Fatalf("README.md %q", b)
	}
	if takeRefusedPath("pkg/.GIT/hooks/pre-commit") == "" || takeRefusedPath(".abhed/config.json") == "" || takeRefusedPath("src/a.go") != "" {
		t.Fatal("takeRefusedPath")
	}
}

func commitAll(t *testing.T, dir string) {
	t.Helper()
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "more")
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := execGit(dir, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
}

func execGit(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	return cmd
}
