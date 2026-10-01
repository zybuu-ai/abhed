//go:build unix

package customcmd

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A file swapped for a FIFO before the open does not hang the read.
func TestOpenCheckedDoesNotWaitOnAFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if f, err := openChecked(p); err == nil {
			_ = f.Close()
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the open waited on a FIFO")
	}
}
