package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseIssueURLs(t *testing.T) {
	cases := map[string]Ref{
		"https://github.com/zybuu-ai/abhed/issues/49":          {"https", "github.com", "zybuu-ai", "abhed", 49},
		"https://gitlab.example.com/group/sub/proj/-/issues/7": {"https", "gitlab.example.com", "group/sub", "proj", 7},
		"https://git.example.com/team/tool/issues/12/":         {"https", "git.example.com", "team", "tool", 12},
		"https://ghe.example.com/org/repo.git/issues/3":        {"https", "ghe.example.com", "org", "repo", 3},
	}
	for raw, want := range cases {
		got, err := Parse(raw)
		if err != nil || got != want {
			t.Errorf("%s: got %+v (%v), want %+v", raw, got, err, want)
		}
	}
	for _, bad := range []string{"not a url", "https://github.com/zybuu-ai/abhed", "https://github.com/zybuu-ai/abhed/issues/x"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// fakeForge records what each API was asked, the way the real hosts expect it.
type fakeForge struct {
	kind  string
	calls []string
	auth  []string
	body  map[string]any
}

func (f *fakeForge) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&f.body)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/version"):
			if f.kind == KindGitea && strings.Contains(r.URL.Path, "/api/v1/") {
				_, _ = w.Write([]byte(`{"version":"1.22"}`))
				return
			}
			if f.kind == KindGitLab && strings.Contains(r.URL.Path, "/api/v4/") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/issues/"):
			_, _ = w.Write([]byte(`{"title":"Fix the thing","body":"It is broken.","description":"It is broken.","html_url":"https://x/issues/5","web_url":"https://x/issues/5"}`))
		case strings.HasSuffix(r.URL.Path, "/pulls") || strings.HasSuffix(r.URL.Path, "/merge_requests"):
			_, _ = w.Write([]byte(`{"html_url":"https://x/pull/9","web_url":"https://x/pull/9"}`))
		default:
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		}
	})
}

func TestEachForgeSpeaksItsOwnAPI(t *testing.T) {
	for _, kind := range []string{KindGitHub, KindGitLab, KindGitea} {
		f := &fakeForge{kind: kind}
		srv := httptest.NewServer(f.handler())
		defer srv.Close()
		ref := Ref{Scheme: "https", Host: "self.example", Owner: "team", Repo: "tool", Number: 5}
		base := map[string]string{KindGitHub: srv.URL + "/api/v3", KindGitLab: srv.URL + "/api/v4", KindGitea: srv.URL + "/api/v1"}[kind]
		fg, err := New(context.Background(), ref, Options{Kind: kind, Token: "tok-secret", APIBase: base, Client: srv.Client()})
		if err != nil {
			t.Fatal(kind, err)
		}
		is, err := fg.Issue(context.Background(), ref)
		if err != nil || is.Title != "Fix the thing" || is.Body != "It is broken." {
			t.Fatalf("%s issue: %+v %v", kind, is, err)
		}
		if b, err := fg.DefaultBranch(context.Background(), ref); err != nil || b != "main" {
			t.Fatalf("%s default branch: %q %v", kind, b, err)
		}
		u, err := fg.OpenPullRequest(context.Background(), ref, PullRequest{Head: "abhed/issue-5", Base: "main", Title: "Resolve #5", Body: "done"})
		if err != nil || u != "https://x/pull/9" {
			t.Fatalf("%s open: %q %v", kind, u, err)
		}
		want := map[string][]string{
			KindGitHub: {"GET /api/v3/repos/team/tool/issues/5", "GET /api/v3/repos/team/tool", "POST /api/v3/repos/team/tool/pulls"},
			KindGitLab: {"GET /api/v4/projects/team%2Ftool/issues/5", "GET /api/v4/projects/team%2Ftool", "POST /api/v4/projects/team%2Ftool/merge_requests"},
			KindGitea:  {"GET /api/v1/repos/team/tool/issues/5", "GET /api/v1/repos/team/tool", "POST /api/v1/repos/team/tool/pulls"},
		}[kind]
		got := f.calls
		for i := range want {
			if i >= len(got) || (got[i] != want[i] && strings.ReplaceAll(got[i], "/", "%2F") != strings.ReplaceAll(want[i], "/", "%2F")) {
				t.Fatalf("%s calls: %v, want %v", kind, got, want)
			}
		}
		for _, a := range f.auth {
			if !strings.Contains(a, "tok-secret") {
				t.Fatalf("%s: request without the token: %q", kind, a)
			}
		}
		head := map[string]string{KindGitHub: "head", KindGitLab: "source_branch", KindGitea: "head"}[kind]
		if f.body[head] != "abhed/issue-5" {
			t.Fatalf("%s pull request body: %v", kind, f.body)
		}
		if fg.PushAuth() == "" || strings.Contains(fg.PushAuth(), "tok-secret") {
			t.Fatalf("%s push auth must be encoded: %q", kind, fg.PushAuth())
		}
	}
}

func TestKindIsInferredFromTheHost(t *testing.T) {
	for _, kind := range []string{KindGitea, KindGitLab, KindGitHub} {
		f := &fakeForge{kind: kind}
		srv := httptest.NewServer(f.handler())
		defer srv.Close()
		u := strings.TrimPrefix(srv.URL, "http://")
		got := inferKind(context.Background(), srv.Client(), Ref{Scheme: "http", Host: u})
		if got != kind {
			t.Errorf("host serving %s inferred as %s", kind, got)
		}
	}
}

func TestNoTokenIsAClearError(t *testing.T) {
	t.Setenv("GITEA_TOKEN", "")
	_, err := New(context.Background(), Ref{Scheme: "https", Host: "git.example"}, Options{Kind: KindGitea, Client: http.DefaultClient})
	if err == nil || !strings.Contains(err.Error(), "abhed secret set GITEA_TOKEN") {
		t.Fatalf("%v", err)
	}
}

// The resolver works on a branch in its own worktree, commits what the
// runner changed, and pushes it to the remote; the checkout is untouched.
func TestResolveWorksInAWorktreeAndPushes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, "", "init", "-q", "--bare", remote)
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "", "clone", "-q", remote, repo)
	run(t, repo, "config", "user.email", "t@example.com")
	run(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "add", "a.txt")
	run(t, repo, "commit", "-q", "-m", "init")
	run(t, repo, "push", "-q", "origin", "HEAD")

	is := Issue{Ref: Ref{Number: 5}, Title: "Add two", URL: "https://x/issues/5"}
	w, err := Begin(ctx, repo, is, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Dir == repo || !strings.Contains(w.Dir, ".abhed/worktrees/issue-5") {
		t.Fatalf("worktree at %s", w.Dir)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "a.txt"), []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha, err := w.Commit(ctx)
	if err != nil || sha == "" {
		t.Fatalf("commit: %q %v", sha, err)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(got) != "one\n" {
		t.Fatalf("the checkout was touched: %q", got)
	}
	if err := w.Push(ctx, "origin", "Basic dXNlcjp0b2s="); err != nil {
		t.Fatal(err)
	}
	if out := run(t, "", "--git-dir", remote, "branch", "--list", "abhed/issue-5"); !strings.Contains(out, "abhed/issue-5") {
		t.Fatalf("branch not on the remote: %q", out)
	}
	if !strings.Contains(w.Diff(ctx), "a.txt") {
		t.Fatalf("diff: %q", w.Diff(ctx))
	}
	w.Cleanup(ctx, repo)
	if _, err := os.Stat(w.Dir); err == nil {
		t.Fatal("worktree not removed")
	}
	// Nothing changed: no commit, and a clear error.
	w2, _ := Begin(ctx, repo, is, "")
	if _, err := w2.Commit(ctx); !errors.Is(err, ErrNoChange) {
		t.Fatalf("empty commit: %v", err)
	}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
