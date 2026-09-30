package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// RestoreFileMode puts the mode on the file it writes, and a link at the
// path leading out of the workspace is refused, not followed.
func TestRestoreFileModeDoesNotFollowALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes do not apply on Windows")
	}
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	outside := filepath.Join(t.TempDir(), "outside")
	_ = os.WriteFile(outside, []byte("theirs"), 0o600)
	sess, err := NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(ws, "run.sh")
	if err := sess.RestoreFileMode(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Lstat(p); !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
		t.Fatalf("restored as %v", info.Mode())
	}
	link := filepath.Join(ws, "link.sh")
	_ = os.Symlink(outside, link)
	if err := sess.RestoreFileMode(link, []byte("x"), 0o755); err == nil {
		t.Fatal("a restore went through a link out of the workspace")
	}
	if info, _ := os.Stat(outside); info.Mode().Perm() != 0o600 {
		t.Fatalf("the file outside became %o", info.Mode().Perm())
	}
	if b, _ := os.ReadFile(outside); string(b) != "theirs" {
		t.Fatal("the file outside was written")
	}
}
