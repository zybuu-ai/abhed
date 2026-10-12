package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// The fingerprint changes with anything git reads as configuration or runs a
// program from, and not with work in the tree or git's objects.
func TestGitFingerprintFollowsWhatGitRuns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	ws := t.TempDir()
	for _, d := range []string{".git/hooks", ".git/info", ".git/objects/ab", "src"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, filepath.FromSlash(rel)), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".git/config", "[core]\n\tbare = false\n")
	write(".git/HEAD", "ref: refs/heads/main\n")
	sub := filepath.Join(ws, "src")
	base := GitFingerprint(sub)
	if base == "" || GitFingerprint(ws) != base {
		t.Fatal("the fingerprint differs between the repository's folders")
	}
	write("src/main.go", "package main\n")
	write(".git/objects/ab/cd", "blob")
	write(".git/HEAD", "ref: refs/heads/other\n")
	if GitFingerprint(sub) != base {
		t.Fatal("work in the tree or git's objects changed the fingerprint")
	}
	for _, change := range []struct{ rel, body string }{
		{".git/config", "[core]\n\tfsmonitor = ./x\n"},
		{".git/hooks/pre-commit", "#!/bin/sh\n"},
		{".git/info/attributes", "* filter=x\n"},
		{".git/commondir", "/tmp\n"},
	} {
		before := GitFingerprint(sub)
		write(change.rel, change.body)
		if GitFingerprint(sub) == before {
			t.Errorf("writing %s left the fingerprint unchanged", change.rel)
		}
	}
	before := GitFingerprint(sub)
	home, _ := os.UserHomeDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\tpager = x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if GitFingerprint(sub) == before {
		t.Error("the global configuration changed and the fingerprint did not")
	}
}

// A linked worktree's fingerprint covers the common git folder it names.
func TestGitFingerprintCoversTheCommonFolder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := t.TempDir()
	wt := t.TempDir()
	gd := filepath.Join(main, ".git", "worktrees", "wt")
	if err := os.MkdirAll(gd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gd, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gd+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := GitFingerprint(wt)
	if err := os.WriteFile(filepath.Join(main, ".git", "config"), []byte("[core]\n\tfsmonitor = x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if GitFingerprint(wt) == before {
		t.Fatal("the common folder's config changed and the worktree's fingerprint did not")
	}
}
