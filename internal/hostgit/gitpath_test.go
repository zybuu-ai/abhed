package hostgit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A git the agent could have planted, in the repository or on its PATH ahead
// of the system's, is never run on the host; the system's is.
func TestGitOnPathInsideTheRepositoryIsRefused(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "sub")
	bin := filepath.Join(repo, "bin")
	for _, d := range []string{filepath.Join(repo, ".git"), sub, bin} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	fake := filepath.Join(bin, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ntouch "+filepath.Join(repo, "pwned")+"\n"), 0o700); err != nil { // #nosec G306 -- an executable fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := GitPath(sub); err == nil || !strings.Contains(err.Error(), "agent's commands can write") {
		t.Fatalf("a git in the repository was accepted: %v", err)
	}
	cmd := Command(context.Background(), sub, "status")
	if err := cmd.Run(); err == nil {
		t.Fatal("the planted git ran")
	}
	if _, err := os.Lstat(filepath.Join(repo, "pwned")); err == nil {
		t.Fatal("the planted git ran")
	}
}
