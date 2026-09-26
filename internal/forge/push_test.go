package forge

import (
	"context"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// gitRepo makes a repository with one commit on a branch named abhed/issue-5,
// with a global configuration of the test's own.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the planted programs are shell commands")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	noWritableAreas(t)
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"},
	} {
		run(t, repo, args...)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "add", "a.txt")
	run(t, repo, "commit", "-q", "-m", "init")
	run(t, repo, "branch", "abhed/issue-5")
	return repo
}

// forgeServer records the Authorization header of every request; it is no
// git server, so the push itself fails.
func forgeServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

const token = "Basic c2VjcmV0LXRva2Vu"

// The forge gets the token, over TLS checked against -ca.
func TestPushSendsTheTokenToTheForge(t *testing.T) {
	pushesTheToken(t, gitRepo(t))
}

// An operator who hardened git with safe.bareRepository=explicit can still
// push: the temporary repository is named, not found.
func TestPushWorksWithExplicitBareRepositories(t *testing.T) {
	repo := gitRepo(t)
	run(t, repo, "config", "--global", "safe.bareRepository", "explicit")
	pushesTheToken(t, repo)
}

// noWritableAreas treats the test's temp folders as outside the sandbox's
// writable areas, as a real home directory is.
func noWritableAreas(t *testing.T) {
	t.Helper()
	old := writableAreas
	writableAreas = func() []string { return nil }
	t.Cleanup(func() { writableAreas = old })
}

func areas(t *testing.T, dirs ...string) {
	t.Helper()
	writableAreas = func() []string { return dirs }
}

// A global git configuration the run could have written is not trusted: in a
// writable area, or in the worktree. One elsewhere in the checkout, as with a
// home directory of dotfiles, is.
func TestPushRefusesAGlobalConfigTheRunCanWrite(t *testing.T) {
	repo := gitRepo(t)
	ref := Ref{Scheme: "https", Host: "git.example", Owner: "t", Repo: "r", Number: 5}
	home := os.Getenv("HOME")
	areas(t, home)
	err := (&Work{Branch: "abhed/issue-5", Dir: t.TempDir()}).Push(context.Background(), repo, ref, token, "")
	if err == nil || !strings.Contains(err.Error(), "global git configuration") {
		t.Fatalf("a global configuration in a writable area was trusted: %v", err)
	}
	areas(t)
	worktree := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", worktree)
	err = (&Work{Branch: "abhed/issue-5", Dir: worktree}).Push(context.Background(), repo, ref, token, "")
	if err == nil || !strings.Contains(err.Error(), "global git configuration") {
		t.Fatalf("a global configuration in the worktree was trusted: %v", err)
	}
}

// A dotfiles checkout of the home directory pushes: the run writes only its
// worktree, not ~/.gitconfig.
func TestPushAllowsAGlobalConfigElsewhereInTheCheckout(t *testing.T) {
	repo := gitRepo(t)
	t.Setenv("HOME", repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitconfig"), []byte("[user]\n\tname = t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pushesTheToken(t, repo)
}

// The push repository is made under ~/.abhed/push and removed after.
func TestPushRepositoryIsMadeUnderAbhedState(t *testing.T) {
	repo := gitRepo(t)
	parent := filepath.Join(os.Getenv("HOME"), ".abhed", "push")
	var during []os.DirEntry
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		during, _ = os.ReadDir(parent)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := Ref{Scheme: "https", Host: strings.TrimPrefix(srv.URL, "https://"), Owner: "t", Repo: "r", Number: 5}
	_ = (&Work{Branch: "abhed/issue-5", Dir: t.TempDir()}).Push(context.Background(), repo, ref, token, ca)
	if len(during) != 1 || !strings.HasPrefix(during[0].Name(), "abhed-push-") {
		t.Fatalf("the push repository was not under %s: %v", parent, during)
	}
	if after, _ := os.ReadDir(parent); len(after) != 0 {
		t.Fatalf("the push repository was left behind: %v", after)
	}
	if info, err := os.Lstat(parent); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("%s is not private: %v %v", parent, info.Mode(), err)
	}
}

// The push fails closed when ~/.abhed/push is somewhere the run can write, or
// is not a private folder of its own.
func TestPushRefusesAnUnsafePushFolder(t *testing.T) {
	repo := gitRepo(t)
	home := os.Getenv("HOME")
	ref := Ref{Scheme: "https", Host: "git.example", Owner: "t", Repo: "r", Number: 5}
	w := &Work{Branch: "abhed/issue-5", Dir: t.TempDir()}
	areas(t, filepath.Join(home, ".abhed"))
	if err := w.Push(context.Background(), repo, ref, token, ""); err == nil || !strings.Contains(err.Error(), ".abhed/push") {
		t.Fatalf("a push folder in a writable area was used: %v", err)
	}
	areas(t)
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(home, ".abhed", "push")); err != nil {
		t.Fatal(err)
	}
	if err := w.Push(context.Background(), repo, ref, token, ""); err == nil || !strings.Contains(err.Error(), "not a file or a link") {
		t.Fatalf("a linked push folder was used: %v", err)
	}
}

// A push folder that is not the operator's own is refused.
func TestPushRefusesAForeignPushFolder(t *testing.T) {
	repo := gitRepo(t)
	if runtime.GOOS != "windows" && os.Getuid() != 0 && fileOwnedByMe(mustLstat(t, "/")) {
		t.Fatal("/ reads as owned by this user")
	}
	old := ownedByMe
	ownedByMe = func(os.FileInfo) bool { return false }
	t.Cleanup(func() { ownedByMe = old })
	ref := Ref{Scheme: "https", Host: "git.example", Owner: "t", Repo: "r", Number: 5}
	err := (&Work{Branch: "abhed/issue-5", Dir: t.TempDir()}).Push(context.Background(), repo, ref, token, "")
	if err == nil || !strings.Contains(err.Error(), "of your own") {
		t.Fatalf("a push folder owned by another user was used: %v", err)
	}
}

func mustLstat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// On a disk that ignores case, a worktree named in another case is still the
// worktree: a global configuration inside it is refused.
func TestPushComparesPathsWithoutCase(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("names are case-sensitive here")
	}
	repo := gitRepo(t)
	worktree := t.TempDir()
	if _, err := os.Stat(strings.ToUpper(worktree)); err != nil {
		t.Skip("this disk is case-sensitive")
	}
	t.Setenv("XDG_CONFIG_HOME", worktree)
	ref := Ref{Scheme: "https", Host: "git.example", Owner: "t", Repo: "r", Number: 5}
	err := (&Work{Branch: "abhed/issue-5", Dir: strings.ToUpper(worktree)}).Push(context.Background(), repo, ref, token, "")
	if err == nil || !strings.Contains(err.Error(), "global git configuration") {
		t.Fatalf("a configuration in the worktree, named in another case, was trusted: %v", err)
	}
}

// XDG_CONFIG_HOME in a writable area is refused even when HOME is not.
func TestPushRefusesAWritableXDGConfig(t *testing.T) {
	repo := gitRepo(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	areas(t, xdg)
	ref := Ref{Scheme: "https", Host: "git.example", Owner: "t", Repo: "r", Number: 5}
	err := (&Work{Branch: "abhed/issue-5", Dir: t.TempDir()}).Push(context.Background(), repo, ref, token, "")
	if err == nil || !strings.Contains(err.Error(), filepath.Join(xdg, "git", "config")) {
		t.Fatalf("a writable XDG configuration was trusted: %v", err)
	}
}

// A global configuration that is a link to a safe file, in a folder the run
// can write, is refused: the run could swap the link.
func TestPushRefusesALinkedConfigInAWritableFolder(t *testing.T) {
	repo := gitRepo(t)
	xdg, elsewhere := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	target := filepath.Join(elsewhere, "config")
	if err := os.WriteFile(target, []byte("[user]\n\tname = t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(xdg, "git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(xdg, "git", "config")); err != nil {
		t.Fatal(err)
	}
	areas(t, filepath.Join(xdg, "git"))
	ref := Ref{Scheme: "https", Host: "git.example", Owner: "t", Repo: "r", Number: 5}
	err := (&Work{Branch: "abhed/issue-5", Dir: t.TempDir()}).Push(context.Background(), repo, ref, token, "")
	if err == nil || !strings.Contains(err.Error(), "global git configuration") {
		t.Fatalf("a linked configuration in a writable folder was trusted: %v", err)
	}
}

// An existing push folder that others can write is made private again.
func TestPushTightensAnOpenPushFolder(t *testing.T) {
	repo := gitRepo(t)
	parent := filepath.Join(os.Getenv("HOME"), ".abhed", "push")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	ref := Ref{Scheme: "https", Host: "git.example", Owner: "t", Repo: "r", Number: 5}
	_ = (&Work{Branch: "abhed/issue-5", Dir: t.TempDir()}).Push(context.Background(), repo, ref, token, "")
	if info, err := os.Lstat(parent); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the push folder was left open: %v %v", info.Mode(), err)
	}
}

func pushesTheToken(t *testing.T, repo string) {
	t.Helper()
	srv, seen := forgeServer(t)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, cert, 0o600); err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(srv.URL, "https://")
	ref := Ref{Scheme: "https", Host: host, Owner: "t", Repo: "r", Number: 5}
	_ = (&Work{Branch: "abhed/issue-5", Dir: t.TempDir()}).Push(context.Background(), repo, ref, token, ca)
	got := seen()
	if len(got) == 0 || got[0] != token {
		t.Fatalf("the forge did not get the token: %q", got)
	}
}

// The push reads nothing from the repository's own configuration, which the
// agent could have written: not a TLS check turned off, an address resolved
// elsewhere, a rewrite, a transport allowed by name, nor a credential helper.
func TestPushIgnoresTheRepositorysConfiguration(t *testing.T) {
	repo := gitRepo(t)
	srv, seen := forgeServer(t)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	host := "git.example:" + port
	ref := Ref{Scheme: "https", Host: host, Owner: "t", Repo: "r", Number: 5}
	url, _ := PushURL(ref)
	marks := t.TempDir()
	for _, kv := range [][]string{
		{"http." + url + ".sslVerify", "false"},
		{"http." + url + ".curloptResolve", host + ":127.0.0.1"},
		{"credential.helper", "!touch " + filepath.Join(marks, "helper.ran") + "; true"},
		{"protocol.ext.allow", "always"},
		{"protocol.file.allow", "always"},
		{"url.ext::sh -c touch% " + filepath.Join(marks, "ext.ran") + "% #.pushInsteadOf", "https://other.example/"},
	} {
		run(t, repo, "config", kv[0], kv[1])
	}
	w := &Work{Branch: "abhed/issue-5", Dir: t.TempDir()}
	if err := w.Push(context.Background(), repo, ref, token, ""); err == nil {
		t.Fatal("the push reported success")
	}
	for _, h := range seen() {
		if h == token {
			t.Fatal("the token went to a host the repository's configuration chose")
		}
	}
	// And a rewrite to a program transport, allowed by name, runs nothing.
	ref2 := Ref{Scheme: "https", Host: "other.example", Owner: "t", Repo: "r", Number: 5}
	_ = w.Push(context.Background(), repo, ref2, token, "")
	for _, m := range []string{"helper.ran", "ext.ran"} {
		if _, err := os.Stat(filepath.Join(marks, m)); err == nil {
			t.Errorf("a program the repository named ran: %s", m)
		}
	}
	if _, err := PushURL(Ref{Scheme: "http", Host: "git.example", Owner: "t", Repo: "r"}); err == nil {
		t.Error("a push over plain http was allowed")
	}
}

// An operator's own rewrite of https to ssh stops the push with a message
// that says how to keep pushes on https.
func TestPushExplainsAnSSHRewrite(t *testing.T) {
	repo := gitRepo(t)
	run(t, repo, "config", "--global", "url.git@git.example:.insteadOf", "https://git.example/")
	ref := Ref{Scheme: "https", Host: "git.example", Owner: "t", Repo: "r", Number: 5}
	err := (&Work{Branch: "abhed/issue-5", Dir: t.TempDir()}).Push(context.Background(), repo, ref, token, "")
	if err == nil || !strings.Contains(err.Error(), "pushInsteadOf") {
		t.Fatalf("the error does not say what to do: %v", err)
	}
}

// A repository the agent left inside the worktree is refused before git
// enters it: its configuration could name programs.
func TestCommitRefusesAnEmbeddedRepository(t *testing.T) {
	repo := gitRepo(t)
	is := Issue{Ref: Ref{Number: 7}, Title: "x", URL: "https://x/issues/7"}
	w, err := Begin(context.Background(), repo, is, "")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Cleanup(context.Background(), repo)
	nested := filepath.Join(w.Dir, "vendor", "lib")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, nested, "init", "-q")
	marks := t.TempDir()
	run(t, nested, "config", "core.fsmonitor", "touch "+filepath.Join(marks, "fsmonitor.ran"))
	if _, err := w.Commit(context.Background()); !errors.Is(err, ErrEmbeddedRepo) {
		t.Fatalf("commit with an embedded repository: %v", err)
	}
	if _, err := os.Stat(filepath.Join(marks, "fsmonitor.ran")); err == nil {
		t.Fatal("the embedded repository's program ran")
	}
}
