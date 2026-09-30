//go:build unix

package app

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A pipe at the export's path neither blocks the open nor is written to.
func TestOpenExportRefusesAPipe(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := openExport(p)
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an export opened a pipe")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the export's open waited on a pipe")
	}
}
