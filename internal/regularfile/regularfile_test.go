//go:build !windows

package regularfile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOpenTakesOnlyAPlainFile(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := Read(plain, 10); err != nil || string(b) != "ok" {
		t.Fatalf("plain: %q %v", b, err)
	}
	if _, err := Read(plain, 1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
	link := filepath.Join(dir, "zero")
	if err := os.Symlink("/dev/zero", link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, fifo, dir} {
		if _, _, err := Open(p); !errors.Is(err, ErrNotRegular) {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// A plain file swapped for a pipe or a link between the look and the open
// is refused at once, never waited on or followed.
func TestOpenRefusesASwapAfterTheLook(t *testing.T) {
	for _, swap := range []string{"fifo", "link to /dev/zero"} {
		t.Run(swap, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "f")
			if err := os.WriteFile(p, []byte("ok"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := afterLstat
			t.Cleanup(func() { afterLstat = old })
			afterLstat = func(path string) {
				_ = os.Remove(path)
				if swap == "fifo" {
					_ = syscall.Mkfifo(path, 0o600)
				} else {
					_ = os.Symlink("/dev/zero", path)
				}
			}
			done := make(chan error, 1)
			go func() { _, err := Read(p, 1<<20); done <- err }()
			select {
			case err := <-done:
				if !errors.Is(err, ErrNotRegular) {
					t.Fatalf("got %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the open waited on what was swapped in")
			}
		})
	}
}
