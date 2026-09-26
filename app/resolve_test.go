package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/internal/forge"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

type fakeForge struct {
	opened *forge.PullRequest
}

func (f *fakeForge) Kind() string     { return forge.KindGitea }
func (f *fakeForge) PushAuth() string { return "Basic eDp5" }
func (f *fakeForge) Issue(context.Context, forge.Ref) (forge.Issue, error) {
	return forge.Issue{Ref: forge.Ref{Number: 5}, Title: "Add two", Body: "a.txt needs a second line", URL: "https://git.example/t/r/issues/5"}, nil
}
func (f *fakeForge) DefaultBranch(context.Context, forge.Ref) (string, error) { return "main", nil }
func (f *fakeForge) OpenPullRequest(_ context.Context, _ forge.Ref, pr forge.PullRequest) (string, error) {
	f.opened = &pr
	return "https://git.example/t/r/pulls/9", nil
}

func resolveRepo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	sh := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	sh("", "init", "-q", "--bare", remote)
	repo := filepath.Join(t.TempDir(), "repo")
	sh("", "clone", "-q", remote, repo)
	sh(repo, "config", "user.email", "t@example.com")
	sh(repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sh(repo, "add", "a.txt")
	sh(repo, "commit", "-q", "-m", "init")
	sh(repo, "push", "-q", "origin", "HEAD")
	return repo, remote
}

func stubResolve(t *testing.T, fg *fakeForge, change string) {
	t.Helper()
	stubForge(t, fg)
	oldRunner := newResolveRunner
	t.Cleanup(func() { newResolveRunner = oldRunner })
	newResolveRunner = func(_ context.Context, opts abhed.Options) (forge.Runner, error) {
		return func(_ context.Context, dir, prompt string) error {
			if !strings.Contains(prompt, "Add two") || !strings.Contains(prompt, "Do not commit") {
				t.Errorf("prompt: %q", prompt)
			}
			if opts.Workspace != dir {
				t.Errorf("the run's workspace is %s, not the worktree %s", opts.Workspace, dir)
			}
			if !opts.Sandbox {
				t.Error("the run does not ask for the configured sandbox")
			}
			if change == "" {
				return nil
			}
			return os.WriteFile(filepath.Join(dir, "a.txt"), []byte(change), 0o600)
		}, nil
	}
}

// stubForge stands in the forge and pushes to the local remote instead.
func stubForge(t *testing.T, fg *fakeForge) {
	t.Helper()
	oldForge := newForge
	newForge = func(context.Context, forge.Ref, forge.Options) (forge.Forge, error) { return fg, nil }
	oldPush := pushWork
	pushWork = func(_ context.Context, w *forge.Work, repo string, ref forge.Ref, _, _ string) error {
		if ref.Host != "git.example" || ref.Owner != "t" || ref.Repo != "r" {
			t.Errorf("pushed for %+v, not the issue's repository", ref)
		}
		out, err := exec.Command("git", "-C", repo, "push", "-q", "origin", w.Branch).CombinedOutput()
		if err != nil {
			t.Errorf("push: %v\n%s", err, out)
		}
		return err
	}
	t.Cleanup(func() { newForge, pushWork = oldForge, oldPush })
}

func branchOnRemote(t *testing.T, remote string) bool {
	t.Helper()
	out, _ := exec.Command("git", "--git-dir", remote, "branch", "--list", "abhed/issue-5").CombinedOutput()
	return strings.Contains(string(out), "abhed/issue-5")
}

func TestResolveOpensAPullRequestFromAWorktree(t *testing.T) {
	repo, remote := resolveRepo(t)
	fg := &fakeForge{}
	stubResolve(t, fg, "one\ntwo\n")

	if code := resolveCmd(repo, []string{"-y", "https://git.example/t/r/issues/5"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if fg.opened == nil || fg.opened.Head != "abhed/issue-5" || fg.opened.Base != "main" || !strings.Contains(fg.opened.Title, "Resolve #5") {
		t.Fatalf("pull request: %+v", fg.opened)
	}
	if !branchOnRemote(t, remote) {
		t.Fatal("the branch was not pushed")
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(got) != "one\n" {
		t.Fatalf("the checkout was changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, ".abhed-worktrees", "issue-5")); err == nil {
		t.Fatal("the worktree was left behind")
	}
}

// Without -y or an allow rule, and with nobody at a terminal, the request is
// not opened and nothing is pushed; a deny rule refuses it outright.
func TestResolveWillNotOpenAPullRequestUnasked(t *testing.T) {
	repo, remote := resolveRepo(t)
	fg := &fakeForge{}
	stubResolve(t, fg, "one\ntwo\n")

	if code := resolveCmd(repo, []string{"https://git.example/t/r/issues/5"}); code == 0 {
		t.Fatal("opened without approval")
	}
	if fg.opened != nil || branchOnRemote(t, remote) {
		t.Fatal("pushed or opened without approval")
	}

	if err := os.MkdirAll(filepath.Join(repo, ".abhed"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".abhed", "config.json"), []byte(`{"permissions":{"deny":["forge_pr(*)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := resolveCmd(repo, []string{"-y", "https://git.example/t/r/issues/5"}); code == 0 {
		t.Fatal("a deny rule was overridden by -y")
	}
	if fg.opened != nil || branchOnRemote(t, remote) {
		t.Fatal("pushed or opened against a deny rule")
	}
}

// A run that changed nothing leaves no worktree and no branch behind.
func TestResolveReportsNoChange(t *testing.T) {
	repo, remote := resolveRepo(t)
	fg := &fakeForge{}
	stubResolve(t, fg, "")
	if code := resolveCmd(repo, []string{"-y", "https://git.example/t/r/issues/5"}); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if fg.opened != nil || branchOnRemote(t, remote) {
		t.Fatal("an empty run was pushed")
	}
	if _, err := os.Lstat(filepath.Join(repo, ".abhed-worktrees")); err == nil {
		t.Error("the empty worktree was left behind")
	}
	if out, _ := exec.Command("git", "-C", repo, "branch", "--list", "abhed/issue-5").Output(); len(out) != 0 {
		t.Errorf("the branch of an empty run was left behind: %s", out)
	}
}

// The agent itself works in the worktree: a model that rewrites a.txt gets it
// written there, and resolve commits and pushes the change.
func TestResolveCommitsWhatTheAgentWrote(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	repo, remote := resolveRepo(t)
	srv := scriptedEndpoint(t, func(dir string) []string {
		file := filepath.Join(dir, "a.txt")
		return []string{
			toolCall("read", map[string]string{"path": file}),
			toolCall("write", map[string]string{"path": file, "content": "one\ntwo\n"}),
			`{"choices":[{"delta":{"content":"Added the second line."}}]}`,
		}
	})
	cfg := `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + srv.URL + `","model":"m","context_window":8192}}}}`
	if err := os.MkdirAll(filepath.Join(repo, ".abhed"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	fg := &fakeForge{}
	stubForge(t, fg)
	if code := resolveCmd(repo, []string{"-y", "https://git.example/t/r/issues/5"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	out, err := exec.Command("git", "--git-dir", remote, "show", "abhed/issue-5:a.txt").CombinedOutput()
	if err != nil || string(out) != "one\ntwo\n" {
		t.Fatalf("the pushed branch does not hold the agent's change: %v %q", err, out)
	}
	if fg.opened == nil {
		t.Fatal("no pull request was opened")
	}
}

// scriptedEndpoint answers each request with the next frame planned from the
// working directory the system prompt names, then with "done".
func scriptedEndpoint(t *testing.T, frames func(dir string) []string) *httptest.Server {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		dir := ""
		if len(req.Messages) > 0 {
			if _, after, ok := strings.Cut(req.Messages[0].Content, "Working directory: "); ok {
				dir, _, _ = strings.Cut(after, "\n")
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frame := `{"choices":[{"delta":{"content":"done"}}]}`
		if all, i := frames(dir), int(n.Add(1))-1; i < len(all) {
			frame = all[i]
		}
		fmt.Fprintf(w, "data: %s\n\n", frame)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// toolCall is a streamed frame that calls one tool.
func toolCall(name string, args map[string]string) string {
	a, _ := json.Marshal(args)
	call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{"index": 0, "id": "c-" + name, "type": "function",
			"function": map[string]any{"name": name, "arguments": string(a)}}}}}}})
	return string(call)
}

// A run that committed its change itself, though asked not to, has its commit
// pushed rather than its branch deleted as if it had done nothing.
func TestResolveKeepsARunsOwnCommit(t *testing.T) {
	repo, remote := resolveRepo(t)
	fg := &fakeForge{}
	stubForge(t, fg)
	old := newResolveRunner
	t.Cleanup(func() { newResolveRunner = old })
	newResolveRunner = func(context.Context, abhed.Options) (forge.Runner, error) {
		return func(_ context.Context, dir, _ string) error {
			if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o600); err != nil {
				return err
			}
			for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-q", "-m", "mine"}} {
				if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %s", args, out)
				}
			}
			return nil
		}, nil
	}
	if code := resolveCmd(repo, []string{"-y", "https://git.example/t/r/issues/5"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	out, err := exec.Command("git", "--git-dir", remote, "show", "abhed/issue-5:a.txt").CombinedOutput()
	if err != nil || string(out) != "one\ntwo\n" {
		t.Fatalf("the run's own commit was not pushed: %v %q", err, out)
	}
}

// A worktree an earlier version left inside .abhed stops resolve with a
// message that says how to remove it, rather than git's own failure.
func TestResolveNamesALegacyWorktree(t *testing.T) {
	repo, _ := resolveRepo(t)
	stubResolve(t, &fakeForge{}, "one\ntwo\n")
	legacy := filepath.Join(repo, ".abhed", "worktrees", "issue-5")
	if err := os.MkdirAll(legacy, 0o750); err != nil {
		t.Fatal(err)
	}
	code, msg := resolveStderr(t, func() int { return resolveCmd(repo, []string{"-y", "https://git.example/t/r/issues/5"}) })
	if code == 0 || !strings.Contains(msg, "git worktree remove --force "+legacy) {
		t.Fatalf("exit %d: %s", code, msg)
	}
}

// runnerDoing stands in a run that does what fn does in its worktree.
func runnerDoing(t *testing.T, fn func(t *testing.T, dir string)) {
	t.Helper()
	old := newResolveRunner
	t.Cleanup(func() { newResolveRunner = old })
	newResolveRunner = func(context.Context, abhed.Options) (forge.Runner, error) {
		return func(_ context.Context, dir, _ string) error { fn(t, dir); return nil }, nil
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
}

// resolveStderr runs fn and returns its exit code and what it wrote to stderr.
func resolveStderr(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	code := fn()
	os.Stderr = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return code, string(out)
}

// A run that commits somewhere other than its branch, on a detached HEAD or
// another branch, pushes nothing and opens nothing: the branch is what would
// be pushed, and it does not hold the work. The worktree is kept, and the
// message says where the commits are.
func TestResolveRefusesCommitsOffTheBranch(t *testing.T) {
	for name, leave := range map[string][]string{
		"detached HEAD":  {"checkout", "-q", "--detach"},
		"another branch": {"checkout", "-q", "-b", "elsewhere"},
	} {
		t.Run(name, func(t *testing.T) {
			repo, remote := resolveRepo(t)
			fg := &fakeForge{}
			stubForge(t, fg)
			var worktree string
			runnerDoing(t, func(t *testing.T, dir string) {
				worktree = dir
				gitIn(t, dir, leave...)
				if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gitIn(t, dir, "commit", "-q", "-am", "mine")
			})
			code, msg := resolveStderr(t, func() int { return resolveCmd(repo, []string{"-y", "https://git.example/t/r/issues/5"}) })
			if code == 0 || fg.opened != nil || branchOnRemote(t, remote) {
				t.Fatalf("exit %d, opened %v, pushed %v: %s", code, fg.opened, branchOnRemote(t, remote), msg)
			}
			if !strings.Contains(msg, "not on abhed/issue-5") || !strings.Contains(msg, "kept") {
				t.Fatalf("the message does not say where the commits are: %s", msg)
			}
			if _, err := os.Stat(filepath.Join(worktree, "a.txt")); err != nil {
				t.Fatalf("the worktree holding the commits was removed: %v", err)
			}
		})
	}
}

// A run that changed nothing to commit but left ignored files keeps its
// worktree and branch: the files may be what it made.
func TestResolveKeepsAWorktreeHoldingIgnoredFiles(t *testing.T) {
	repo, remote := resolveRepo(t)
	fg := &fakeForge{}
	stubForge(t, fg)
	if err := os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte("*.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var worktree string
	runnerDoing(t, func(t *testing.T, dir string) {
		worktree = dir
		if err := os.WriteFile(filepath.Join(dir, "report.log"), []byte("findings\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	code, msg := resolveStderr(t, func() int { return resolveCmd(repo, []string{"-y", "https://git.example/t/r/issues/5"}) })
	if code != 2 || fg.opened != nil || branchOnRemote(t, remote) || !strings.Contains(msg, "holds ignored files") {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if got, _ := os.ReadFile(filepath.Join(worktree, "report.log")); string(got) != "findings\n" {
		t.Fatalf("the ignored file was lost: %q", got)
	}
	if out, _ := exec.Command("git", "-C", repo, "branch", "--list", "abhed/issue-5").Output(); len(out) == 0 {
		t.Fatal("the branch was deleted")
	}
}

// A branch the run rewrote so it no longer builds on its start is not pushed.
func TestResolveRefusesARewrittenBranch(t *testing.T) {
	repo, remote := resolveRepo(t)
	fg := &fakeForge{}
	stubForge(t, fg)
	runnerDoing(t, func(t *testing.T, dir string) {
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("rewritten\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "commit", "-q", "-a", "--amend", "-m", "history rewritten")
	})
	code, msg := resolveStderr(t, func() int { return resolveCmd(repo, []string{"-y", "https://git.example/t/r/issues/5"}) })
	if code == 0 || fg.opened != nil || branchOnRemote(t, remote) || !strings.Contains(msg, "no longer builds on") {
		t.Fatalf("exit %d: %s", code, msg)
	}
}

// A no-change run whose worktree cannot be checked keeps it and its branch,
// and says the check failed rather than guessing at what it holds.
func TestResolveKeepsAWorktreeItCannotCheck(t *testing.T) {
	repo, _ := resolveRepo(t)
	stubResolve(t, &fakeForge{}, "")
	old := workUntouched
	t.Cleanup(func() { workUntouched = old })
	workUntouched = func(context.Context, *forge.Work) (bool, error) { return false, errors.New("git status failed") }
	code, msg := resolveStderr(t, func() int { return resolveCmd(repo, []string{"-y", "https://git.example/t/r/issues/5"}) })
	if code != 2 || !strings.Contains(msg, "could not be checked (git status failed)") || strings.Contains(msg, "ignored files") {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if _, err := os.Stat(filepath.Join(repo, ".abhed-worktrees", "issue-5")); err != nil {
		t.Fatalf("the worktree was removed: %v", err)
	}
	if out, _ := exec.Command("git", "-C", repo, "branch", "--list", "abhed/issue-5").Output(); len(out) == 0 {
		t.Fatal("the branch was deleted")
	}
}
