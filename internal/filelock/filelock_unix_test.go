//go:build unix

package filelock

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A lock file a repository planted as a link or a pipe is refused at once:
// nothing is made at the link's target, and nothing is waited on.
func TestLockRefusesALinkOrAPipe(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "made-here")
	link := filepath.Join(dir, "users.json.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(link, time.Second, busy); err == nil {
		t.Fatal("locked through a link")
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("a file was made at the link's target")
	}
	fifo := filepath.Join(dir, "pipe.lock")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Lock(fifo, time.Second, busy); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("locked a pipe")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the lock waited on a pipe")
	}
}
