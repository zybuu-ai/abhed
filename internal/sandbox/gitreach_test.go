package sandbox

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitShapes makes, under base, a repository and the three workspaces whose
// git folder lies outside them: a folder inside it, a linked worktree, and a
// submodule checkout. It returns the repository's top and each workspace.
func gitShapes(t *testing.T, base string) (repo string, shapes map[string]string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo = filepath.Join(base, "repo")
	gitRun(t, base, "init", "-q", "-b", "main", repo)
	if err := os.MkdirAll(filepath.Join(repo, "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	for f, body := range map[string]string{"README.md": "top readme\n", "deep/a.txt": "a\n"} {
		if err := os.WriteFile(filepath.Join(repo, f), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "first")
	wt := filepath.Join(base, "wt")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "wt", wt)
	super := filepath.Join(base, "super")
	gitRun(t, base, "init", "-q", "-b", "main", super)
	gitRun(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", repo, "app")
	return repo, map[string]string{
		"subfolder":       filepath.Join(repo, "deep"),
		"linked worktree": wt,
		"submodule":       filepath.Join(super, "app"),
	}
}

// GitReach names the repository's top for a folder inside it, and the git
// folders a worktree's or a submodule's .git file names, and nothing else.
func TestGitReachNamesWhatGitReads(t *testing.T) {
	base := workspace(t)
	repo, shapes := gitShapes(t, base)
	want := map[string][]string{
		"subfolder":       {repo, filepath.Join(repo, ".git")},
		"linked worktree": {filepath.Join(repo, ".git", "worktrees", "wt"), filepath.Join(repo, ".git")},
		"submodule":       {filepath.Join(base, "super", ".git", "modules", "app")},
	}
	for name, ws := range shapes {
		if got := GitReach(ws); !slices.Equal(got, want[name]) {
			t.Errorf("%s: GitReach = %v, want %v", name, got, want[name])
		}
	}
	if got := GitReach(repo); len(got) != 0 {
		t.Errorf("a repository's top reaches %v", got)
	}
	if got := GitReach(filepath.Join(base, "nothing")); len(got) != 0 {
		t.Errorf("a folder in no repository reaches %v", got)
	}
}

// A .git file a command rewrote to name another repository's git folder, or
// a submodule's git folder naming another work tree, reaches nothing; nor
// does a repository that is a home kept in git.
func TestGitReachRefusesWhatDoesNotNameTheWorktreeBack(t *testing.T) {
	base := workspace(t)
	_, shapes := gitShapes(t, base)
	other := filepath.Join(base, "other")
	gitRun(t, base, "init", "-q", "-b", "main", other)
	wt := shapes["linked worktree"]
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(other, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := GitReach(wt); len(got) != 0 {
		t.Errorf("a .git file naming another repository reaches %v", got)
	}
	app := shapes["submodule"]
	gitRun(t, filepath.Join(base, "super", ".git", "modules", "app"), "config", "core.worktree", other)
	if got := GitReach(app); len(got) != 0 {
		t.Errorf("a submodule git folder naming another work tree reaches %v", got)
	}
	// A home kept in git: its git folder would show the home.
	t.Setenv("HOME", filepath.Join(base, "repo"))
	if got := GitReach(shapes["subfolder"]); len(got) != 0 {
		t.Errorf("a repository whose top is a home reaches %v", got)
	}
}

// reachSandbox is the process tier for ws, with git's files held as every
// session holds them.
func reachSandbox(t *testing.T, ws string) *Process {
	t.Helper()
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// gitFolders are each shape's git folder and common git folder.
func gitFolders(base, repo, shape string) (gd, common string) {
	switch shape {
	case "linked worktree":
		return filepath.Join(repo, ".git", "worktrees", "wt"), filepath.Join(repo, ".git")
	case "submodule":
		g := filepath.Join(base, "super", ".git", "modules", "app")
		return g, g
	}
	return filepath.Join(repo, ".git"), filepath.Join(repo, ".git")
}

// reachBase is where a test makes its repositories: a home the sandbox keeps
// unreadable but for the workspace, as a person's projects under ~ are.
// Seatbelt lets commands write the account's temp folder, where a test's
// folders are, so on macOS a write a rule refuses is seen refused only in
// a folder under the real home: with ABHED_TEST_GIT_UNDER_HOME=1 the test
// makes one there (and removes it); without it, the rules are read instead.
func reachBase(t *testing.T) (home string, refusalsShow bool) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the process tier runs on macOS and Linux")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	if runtime.GOOS == "darwin" && os.Getenv("ABHED_TEST_GIT_UNDER_HOME") == "1" {
		u, err := user.Current()
		if err != nil || u.HomeDir == "" {
			t.Fatalf("no account home: %v", err)
		}
		real := u.HomeDir
		dir, err := os.MkdirTemp(real, "abhed-gitreach-test-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir, true
	}
	home = filepath.Join(workspace(t), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return home, runtime.GOOS == "linux"
}

const commitLine = "echo x > made-in-sandbox.txt && git add made-in-sandbox.txt && " +
	"git -c user.name=t -c user.email=t@example.test commit -q -m in-sandbox && git log -1 --format=%s"

// On the process tier, git reads in a workspace whose git folder lies
// outside it, even under a home the sandbox otherwise keeps unreadable, and
// commits where that git folder serves the workspace alone (a submodule's).
// A git folder shared with another work tree, a linked worktree's common
// folder or a repository's .git for a folder inside it, is never written: no
// commit there, and none of what a command could do through it to another
// work tree or the person's history.
func TestProcessSandboxReachesAGitFolderOutsideTheWorkspace(t *testing.T) {
	home, refusalsShow := reachBase(t)
	repo, shapes := gitShapes(t, home)
	gitRun(t, repo, "branch", "keep")
	for name, ws := range shapes {
		t.Run(name, func(t *testing.T) {
			s := reachSandbox(t, ws)
			if out, err := runIn(t, s, ws, "git status --short && git log -1 --format=%s"); err != nil || !strings.Contains(out, "first") {
				t.Fatalf("git in the sandbox: %v\n%s", err, out)
			}
			if name == "subfolder" {
				if out, err := runIn(t, s, ws, "cat ../README.md"); err != nil || !strings.Contains(out, "top readme") {
					t.Fatalf("the repository's top was not readable: %v\n%s", err, out)
				}
			}
			if n := s.gitReachNote(); n == "" {
				t.Error("the description does not say what outside the workspace git reaches")
			}
			gd, common := gitFolders(home, repo, name)
			profile := ""
			if s.backend == "sandbox-exec" {
				profile = s.seatbeltProfile()
			}
			if name == "submodule" {
				if out, err := runIn(t, s, ws, commitLine); err != nil || !strings.Contains(out, "in-sandbox") {
					t.Fatalf("a commit in the submodule: %v\n%s", err, out)
				}
				ownRefusals(t, s, ws, gd, refusalsShow)
				return
			}
			// Shared folders: read, never written.
			for _, f := range formsOf([]string{common, gd}) {
				if profile != "" && strings.Contains(profile, "(allow file-write* (subpath "+quote(f)+"))") {
					t.Errorf("the profile lets commands write %s", f)
				}
			}
			if !refusalsShow {
				return
			}
			before := snapshot(t, common)
			for _, attempt := range []string{
				commitLine,
				"git update-ref refs/heads/keep HEAD~0",
				"git update-ref refs/heads/main HEAD",
				"echo x >> " + filepath.Join(common, "index"),
				"echo ref: refs/heads/keep > " + filepath.Join(common, "HEAD"),
				"git replace $(git rev-parse HEAD) $(git rev-parse HEAD)^{tree} 2>&1 || git replace -f HEAD HEAD",
				"mkdir -p " + filepath.Join(common, "info") + " && echo x > " + filepath.Join(common, "info", "grafts"),
				"echo '/*' > " + filepath.Join(common, "info", "sparse-checkout"),
				"echo http://evil.example/ > " + filepath.Join(common, "objects", "info", "http-alternates"),
				"git update-index --add --cacheinfo 100644,$(git hash-object -w ../README.md 2>/dev/null || echo e69de29bb2d1d6434b8b29ae775ad8c2e48c5391),other/main.go",
				"echo x > " + filepath.Join(common, "hooks", "pre-commit"),
				"echo '[core]' >> " + filepath.Join(common, "config"),
			} {
				if out, err := runIn(t, s, ws, attempt); err == nil {
					t.Errorf("%s: the sandbox let a command run %q: %s", name, attempt, out)
				}
			}
			if after := snapshot(t, common); after != before {
				t.Errorf("%s: the shared git folder changed:\n%s", name, diffLines(before, after))
			}
		})
	}
}

// ownRefusals checks a submodule's own git folder gd, which commands write:
// what git reads as configuration, programs or history in it stays held.
func ownRefusals(t *testing.T, s *Process, ws, gd string, refusalsShow bool) {
	t.Helper()
	if s.backend == "sandbox-exec" {
		profile := s.seatbeltProfile()
		for _, want := range []string{gitFolderPattern(gd), gitExtrasPattern(gd)} {
			if !strings.Contains(profile, want) {
				t.Errorf("the profile lacks %s", want)
			}
		}
	}
	// Abhed holds these without making them: git would print a hint for each
	// host git status.
	for _, f := range []string{"info/grafts", "info/sparse-checkout", "objects/info/http-alternates"} {
		if exists(filepath.Join(gd, filepath.FromSlash(f))) {
			t.Errorf("%s was made in the person's git folder", f)
		}
	}
	if !refusalsShow {
		return
	}
	for _, attempt := range []string{
		"echo x > " + filepath.Join(gd, "hooks", "pre-commit"),
		"echo '[core]' >> " + filepath.Join(gd, "config"),
		"git config core.hooksPath " + ws,
		"mv " + filepath.Join(gd, "hooks") + " " + filepath.Join(gd, "hooks-moved"),
		"git replace -f HEAD HEAD~1",
		"echo x > " + filepath.Join(gd, "info", "grafts"),
		"echo '/*' > " + filepath.Join(gd, "info", "sparse-checkout"),
		"echo http://evil.example/ > " + filepath.Join(gd, "objects", "info", "http-alternates"),
		"echo /elsewhere > " + filepath.Join(gd, "commondir") + " && test -s " + filepath.Join(gd, "commondir"),
	} {
		if out, err := runIn(t, s, ws, attempt); err == nil && !strings.Contains(attempt, "commondir") {
			t.Errorf("the sandbox let a command run %q: %s", attempt, out)
		}
	}
	// A commondir made there is moved aside soon after it is made, before any next
	// command, where the person's own git could follow it.
	if s.backend == "bwrap" {
		_, _ = runIn(t, s, ws, "echo /elsewhere > "+filepath.Join(gd, "commondir"))
		time.Sleep(300 * time.Millisecond)
		if exists(filepath.Join(gd, "commondir")) {
			t.Error("a commondir made in the submodule's git folder stayed after the command")
		}
		_, _ = runIn(t, s, ws, "true") // refused, saying why
	}
	// A shallow list or commondir bubblewrap could not hold is taken out
	// before the next command, which is refused.
	_, _ = runIn(t, s, ws, "git rev-parse HEAD > "+filepath.Join(gd, "shallow"))
	_, _ = runIn(t, s, ws, "true")
	for _, f := range []string{"shallow", "commondir"} {
		if b, err := os.ReadFile(filepath.Join(gd, f)); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			t.Errorf("a %s planted in the submodule's git folder stayed", f)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(gd, "refs", "replace")); len(entries) > 0 {
		t.Error("a replace ref was planted")
	}
	if out, err := runIn(t, s, ws, "git log -1 --format=%s"); err != nil || !strings.Contains(out, "in-sandbox") {
		t.Errorf("git after the plants were taken out: %v\n%s", err, out)
	}
}

// snapshot lists every file under dir with its size and contents' sum.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, _ := os.ReadFile(p)
		fmt.Fprintf(&b, "%s %d %x\n", strings.TrimPrefix(p, dir), len(data), sha256.Sum256(data))
		return nil
	})
	return b.String()
}

func diffLines(a, b string) string {
	have := map[string]bool{}
	for _, l := range strings.Split(a, "\n") {
		have[l] = true
	}
	var out []string
	for _, l := range strings.Split(b, "\n") {
		if !have[l] {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// Reading a repository's top never reaches Abhed state in it: a top that
// holds another workspace's .abhed is not read at all (only its git folder),
// and on macOS one made later, in any case, stays unreadable.
func TestTheRepositoryTopKeepsOtherStateOut(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the process tier runs on macOS and Linux")
	}
	base := workspace(t)
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo, shapes := gitShapes(t, home)
	ws := shapes["subfolder"]
	other := filepath.Join(repo, "other", stateDir)
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "record"), []byte("NESTED-STATE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := reachSandbox(t, ws)
	if out, err := runIn(t, s, ws, "cat ../other/.abhed/record"); err == nil || strings.Contains(out, "NESTED-STATE") {
		t.Fatalf("another workspace's state was read: %s", out)
	}
	if r := s.reach(); r.top != "" {
		t.Errorf("a top holding another .abhed was read: %+v", r)
	}
	if out, err := runIn(t, s, ws, "git log -1 --format=%s"); err != nil || !strings.Contains(out, "first") {
		t.Errorf("git log without the top: %v\n%s", err, out)
	}

	if runtime.GOOS != "darwin" {
		return
	}
	// Made after the sandbox read the top: denied by pattern, in any case.
	if err := os.RemoveAll(filepath.Join(repo, "other")); err != nil {
		t.Fatal(err)
	}
	s2 := reachSandbox(t, ws)
	if _, err := runIn(t, s2, ws, "true"); err != nil {
		t.Fatal(err)
	}
	if r := s2.reach(); r.top == "" {
		t.Fatal("the top was not read")
	}
	later := filepath.Join(repo, "other", ".ABHED")
	if err := os.MkdirAll(later, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(later, "record"), []byte("NESTED-STATE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../other/.ABHED/record", "../OTHER/.abhed/record", "../other/.abhed/record"} {
		if out, err := runIn(t, s2, ws, "cat "+p); err == nil || strings.Contains(out, "NESTED-STATE") {
			t.Errorf("%s was read: %s", p, out)
		}
	}
}
