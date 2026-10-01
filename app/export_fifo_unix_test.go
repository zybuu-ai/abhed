//go:build unix

package app

import (
	"os"
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

// Swapped between the check and the open: a hard link to another file is
// refused before anything is truncated, and a pipe does not stall the open.
func TestOpenExportSwappedAfterTheCheck(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	_ = os.WriteFile(victim, []byte("keep"), 0o600)
	target := filepath.Join(dir, "out.jsonl")
	_ = os.WriteFile(target, []byte("old"), 0o600)
	defer func() { exportSwap = func(string) {} }()

	exportSwap = func(p string) { _ = os.Remove(p); _ = os.Link(victim, p) }
	if f, err := openExport(target); err == nil {
		_ = f.Close()
		t.Fatal("a hard link swapped in was written")
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("the file swapped in was truncated: %q", b)
	}

	_ = os.Remove(target)
	_ = os.WriteFile(target, []byte("old"), 0o600)
	exportSwap = func(p string) { _ = os.Remove(p); _ = syscall.Mkfifo(p, 0o600) }
	done := make(chan error, 1)
	go func() {
		f, err := openExport(target)
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a pipe swapped in was opened")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the open waited on a pipe swapped in")
	}
}
