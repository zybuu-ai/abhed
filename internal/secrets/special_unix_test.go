//go:build unix

package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO or a device at the store's path is refused at once, never read: a
// FIFO would block the start and /dev/zero would never end.
func TestSpecialFilesAreRefusedWithoutReading(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	zero := filepath.Join(dir, "zero.json")
	if err := os.Symlink("/dev/zero", zero); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, zero} {
		done := make(chan error, 1)
		go func() { _, err := Open(path).LoadRedactor(); done <- err }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("%s: want a refusal, got %v", path, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: loading the store blocked", path)
		}
	}
}
