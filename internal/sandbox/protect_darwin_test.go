package sandbox

// Only seatbelt names a path that does not exist yet or matches by pattern;
// these are built on macOS alone, so Linux CI has no skip to account for.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Where .git does not exist yet, a command cannot make one that points at a
// git folder it controls. Only seatbelt can name a path that does not exist.
func TestProcessSandboxRefusesANewGitFile(t *testing.T) {
	ws := workspace(t)
	p := DefaultPolicy(ws)
	p.WriteProtected = []string{filepath.Join(ws, ".git", "config"), filepath.Join(ws, ".git", "hooks")}
	s := NewProcess(p)
	available(t, s)
	_, _ = runIn(t, s, ws, "mkdir -p .git2/hooks; printf 'gitdir: .git2\\n' > .git")
	if _, err := os.Lstat(filepath.Join(ws, ".git")); err == nil {
		t.Fatal("the command made a .git of its own")
	}
}

// With ProtectGit, seatbelt holds every git folder's config and hooks, and
// each .git, at any depth and in any case, including repositories made later.
func TestProcessSandboxProtectsNestedRepositories(t *testing.T) {
	ws := workspace(t)
	if err := os.MkdirAll(filepath.Join(ws, "sub", ".git", "hooks"), 0o750); err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	available(t, s)
	_, _ = runIn(t, s, ws, "touch sub/.git/hooks/pre-commit; echo x > sub/.GIT/config; mv sub/.git sub/g2; mkdir -p new/.Git/HOOKS; printf 'gitdir: x\\n' > other.git; mkdir d; printf 'gitdir: ../x\\n' > d/.git")
	for _, f := range []string{"sub/.git/hooks/pre-commit", "sub/.git/config", "sub/g2", "new/.Git", "d/.git"} {
		if _, err := os.Lstat(filepath.Join(ws, filepath.FromSlash(f))); err == nil {
			t.Errorf("the command made %s", f)
		}
	}
	if out, err := runIn(t, s, ws, "touch sub/.git/HEAD sub/main.go"); err != nil {
		t.Fatalf("the rest of a nested repository is not writable: %v %s", err, out)
	}
}

// A command can read no credential under home, nor ~/.abhed, when home is
// reached through a link: seatbelt matches the path the kernel resolved.
func TestProcessSandboxDeniesHomeSecretsThroughALink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", link)
	files := []string{".netrc", ".git-credentials", ".npmrc", ".config/gh/hosts.yml", ".abhed/secrets.json", ".zsh_history"}
	for _, f := range files {
		p := filepath.Join(real, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("CANARY-"+f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(real, ".gitconfig"), []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := workspace(t)
	s := NewProcess(DefaultPolicy(ws))
	available(t, s)
	for _, f := range files {
		for _, home := range []string{link, real} {
			out, _ := runIn(t, s, ws, "cat "+filepath.Join(home, filepath.FromSlash(f)))
			if strings.Contains(out, "CANARY") {
				t.Errorf("read %s through %s: %s", f, home, out)
			}
		}
	}
	if out, err := runIn(t, s, ws, "cat "+filepath.Join(link, ".gitconfig")); err != nil || !strings.Contains(out, "plain") {
		t.Errorf("an ordinary file in home was refused: %v %s", err, out)
	}
}
