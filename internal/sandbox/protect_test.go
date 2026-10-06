package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// editorRepo is a workspace with a git folder and the editor's files in it,
// and the policy that protects them as Studio asks.
func editorRepo(t *testing.T, ws string) Policy {
	t.Helper()
	for _, d := range []string{".git/hooks", ".vscode"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".git/config", ".git/HEAD"} {
		if err := os.WriteFile(filepath.Join(ws, f), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := DefaultPolicy(ws)
	for _, f := range []string{".git/config", ".git/hooks", ".vscode"} {
		p.WriteProtected = append(p.WriteProtected, filepath.Join(ws, f))
	}
	return p
}

func available(t *testing.T, s Sandbox) {
	t.Helper()
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
}

// Renaming or removing .git would carry its config and hooks out from under
// their rules; the folder stays where it is, and what else it holds is writable.
func TestProcessSandboxPinsTheGitFolder(t *testing.T) {
	requireNetNS(t)
	ws := workspace(t)
	s := NewProcess(editorRepo(t, ws))
	available(t, s)
	_, _ = runIn(t, s, ws, "mv .git .git2; mv .vscode v2; rm -rf .git; printf 'gitdir: .git2\\n' > .git")
	if info, err := os.Stat(filepath.Join(ws, ".git", "config")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf(".git was moved or removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".vscode")); err != nil {
		t.Fatalf(".vscode was moved: %v", err)
	}
	if out, err := runIn(t, s, ws, "printf 'ref: x\\n' > .git/HEAD"); err != nil {
		t.Fatalf("the rest of .git is not writable: %v %s", err, out)
	}
}

// A git folder named by a .git file is protected as the workspace's own: its
// hooks cannot be written, the folder cannot be moved, nor the file re-pointed.
func TestProcessSandboxProtectsTheGitFolderAGitFileNames(t *testing.T) {
	requireNetNS(t)
	ws := workspace(t)
	if err := os.MkdirAll(filepath.Join(ws, ".git2", "hooks"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git"), []byte("gitdir: .git2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy(ws)
	for _, f := range []string{".git/config", ".git/hooks", ".git2/config", ".git2/hooks"} {
		p.WriteProtected = append(p.WriteProtected, filepath.Join(ws, f))
	}
	s := NewProcess(p)
	available(t, s)
	_, _ = runIn(t, s, ws, "touch .git2/hooks/pre-push; mv .git2 .git3; printf 'gitdir: .git3\\n' > .git")
	if _, err := os.Stat(filepath.Join(ws, ".git2", "hooks", "pre-push")); err == nil {
		t.Fatal("a hook was written in the git folder the .git file names")
	}
	if b, err := os.ReadFile(filepath.Join(ws, ".git")); err != nil || string(b) != "gitdir: .git2\n" {
		t.Fatalf(".git was re-pointed: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git2")); err != nil {
		t.Fatalf("the git folder was moved: %v", err)
	}
}

// A workspace given by a path through a link, as /tmp is on macOS, is
// protected at its real path as well as the one given.
func TestProcessSandboxProtectsThroughALinkedWorkspace(t *testing.T) {
	requireNetNS(t)
	real := workspace(t)
	link := filepath.Join(workspace(t), "ws")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	editorRepo(t, real)
	p := DefaultPolicy(link)
	for _, f := range []string{".git/config", ".git/hooks", ".vscode"} {
		p.WriteProtected = append(p.WriteProtected, filepath.Join(link, f))
	}
	s := NewProcess(p)
	available(t, s)
	_, _ = runIn(t, s, link, "touch .git/hooks/pre-commit "+filepath.Join(real, ".git/hooks/post-commit")+"; echo hi > .vscode/tasks.json")
	for _, f := range []string{".git/hooks/pre-commit", ".git/hooks/post-commit", ".vscode/tasks.json"} {
		if _, err := os.Stat(filepath.Join(real, f)); err == nil {
			t.Errorf("wrote %s through the linked workspace", f)
		}
	}
	if out, err := runIn(t, s, link, "echo ok > notes.txt"); err != nil {
		t.Fatalf("the workspace is not writable through its link: %v %s", err, out)
	}
}

// Under bubblewrap and in a container the folders holding a protected path
// are mounted onto themselves before the read-only mounts, both forms of each
// path are mounted, and a path outside the workspace is not shown.
func TestMountsPinHoldersAndBothForms(t *testing.T) {
	real := workspace(t)
	link := filepath.Join(workspace(t), "ws")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	editorRepo(t, real)
	outside := filepath.Join(workspace(t), "hooks")
	p := DefaultPolicy(link)
	p.WriteProtected = []string{filepath.Join(link, ".git", "hooks"), outside}
	hooks, gitDir := filepath.Join(real, ".git", "hooks"), filepath.Join(real, ".git")
	check := func(name, args, pin, ro string) {
		t.Helper()
		at := strings.Index(args, pin)
		if at < 0 || !strings.Contains(args, ro) || strings.Index(args, ro) < at {
			t.Errorf("%s: want %q before %q in\n%s", name, pin, ro, args)
		}
		if strings.Contains(args, outside) {
			t.Errorf("%s: a path outside the workspace is mounted:\n%s", name, args)
		}
	}
	b := &Process{policy: p, backend: "bwrap"}
	check("bwrap", strings.Join(b.wrap(t.Context(), link, nil, "/bin/true").Args, " "),
		"--bind "+gitDir+" "+gitDir, "--ro-bind-try "+hooks+" "+hooks)
	c := &Container{policy: p}
	check("container", strings.Join(c.Command(context.Background(), link, "true").Args, " "),
		"-v "+gitDir+":"+gitDir+" ", "-v "+hooks+":"+hooks+":ro")
}

// The seatbelt profile names the workspace and its protected paths in both forms.
func TestSeatbeltProfileNamesBothForms(t *testing.T) {
	real := workspace(t)
	link := filepath.Join(workspace(t), "ws")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy(link)
	p.WriteProtected = []string{filepath.Join(link, ".git", "hooks")}
	profile := NewProcess(p).seatbeltProfile()
	for _, want := range []string{
		`(allow file-write* (subpath "` + real + `"))`,
		`(deny file-write* (subpath "` + filepath.Join(real, stateDir) + `"))`,
		`(deny file-write* (subpath "` + filepath.Join(link, ".git", "hooks") + `"))`,
		`(deny file-write* (subpath "` + filepath.Join(real, ".git", "hooks") + `"))`,
		`(deny file-write* (literal "` + filepath.Join(real, ".git") + `"))`,
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// A nested repository's holder is its .git, not the folders above it, so
// those can still be renamed.
func TestHoldersStopAtTheNearestGit(t *testing.T) {
	ws := "/ws"
	got := holders([]string{ws}, []string{"/ws/a/b/.git/hooks", "/ws/.git/config", "/ws/store/x.git/hooks"})
	want := []string{"/ws/a/b/.git", "/ws/.git", "/ws/store", "/ws/store/x.git"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("holders %v, want %v", got, want)
	}
}

// The pattern is the workspace quoted, with .git, a worktree's or a
// submodule's folder, and each pointer in any case.
func TestSeatbeltGitPatternQuotesTheWorkspace(t *testing.T) {
	p := DefaultPolicy("/w.s+x")
	p.ProtectGit = true
	profile := NewProcess(p).seatbeltProfile()
	if !strings.Contains(profile, `(regex #"^/w\.s\+x/(.+/)?\.[gG][iI][tT]/(([wW][oO][rR][kK][tT][rR][eE][eE][sS]/[^/]+|[mM][oO][dD][uU][lL][eE][sS]/.+)/)?(`) ||
		!strings.Contains(profile, `[cC][oO][mM][mM][oO][nN][dD][iI][rR]|`) {
		t.Fatalf("%s", profile)
	}
	if strings.Contains(NewProcess(DefaultPolicy("/w")).seatbeltProfile(), "regex #\"^/w/") {
		t.Fatal("the pattern is there without ProtectGit")
	}
}

// With ProtectGit, every git folder's config and hooks are kept from a
// command on both Seatbelt (by pattern) and bubblewrap (by the folders found
// when the command starts); the rest of the repository is writable.
func TestProcessSandboxProtectGitHoldsOnEveryBackend(t *testing.T) {
	requireNetNS(t)
	ws := workspace(t)
	for _, d := range []string{".git/hooks", "sub/.git/hooks"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".git/config", "sub/.git/config"} {
		if err := os.WriteFile(filepath.Join(ws, f), []byte("[core]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	available(t, s)
	_, _ = runIn(t, s, ws, "echo x > .git/hooks/pre-commit; echo '[core] hooksPath=/tmp' >> .git/config; echo x > sub/.git/hooks/post-checkout; echo x >> sub/.git/config")
	for _, f := range []string{".git/hooks/pre-commit", "sub/.git/hooks/post-checkout"} {
		if _, err := os.Lstat(filepath.Join(ws, filepath.FromSlash(f))); err == nil {
			t.Errorf("the command wrote %s", f)
		}
	}
	for _, f := range []string{".git/config", "sub/.git/config"} {
		if data, _ := os.ReadFile(filepath.Join(ws, filepath.FromSlash(f))); string(data) != "[core]\n" {
			t.Errorf("the command changed %s: %q", f, data)
		}
	}
	if out, err := runIn(t, s, ws, "echo ok > sub/main.go"); err != nil {
		t.Fatalf("the repository is not writable: %v %s", err, out)
	}
}

// The walk finds each git folder's config and hooks, and a .git file, and
// passes over Abhed's state and node_modules.
func TestGitProtectedFindsGitFolders(t *testing.T) {
	ws := workspace(t)
	for _, d := range []string{".git/hooks", "a/b/.git/hooks", "node_modules/x/.git/hooks", ".abhed/y/.git/hooks", "wt"} {
		if err := os.MkdirAll(filepath.Join(ws, filepath.FromSlash(d)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".git/config", "wt/.git"} {
		if err := os.WriteFile(filepath.Join(ws, filepath.FromSlash(f)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]bool{}
	for _, p := range GitProtected(ws) {
		rel, _ := filepath.Rel(ws, p)
		got[filepath.ToSlash(rel)] = true
	}
	for _, want := range []string{".git/config", ".git/hooks", "a/b/.git/hooks", "wt/.git"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	for bad := range got {
		if strings.HasPrefix(bad, "node_modules") || strings.HasPrefix(bad, ".abhed") {
			t.Errorf("walked into %s", bad)
		}
	}
}
