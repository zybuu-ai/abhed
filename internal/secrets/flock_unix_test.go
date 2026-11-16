//go:build unix

package secrets

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A write waits for a file lock another holder (another process, in use)
// has on the store.
func TestWriteWaitsForTheFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	unlock, err := fileLock(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Open(path).Set("NAME", "value-123456") }()
	select {
	case err := <-done:
		t.Fatalf("Set did not wait for the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The lock across processes gives up when its context ends, rather than
// waiting on a holder that never lets go.
func TestFileLockContextGivesUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held")
	unlock, err := fileLock(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := fileLockContext(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("taken while held, or not given up: %v", err)
	}
	unlock()
	again, err := fileLockContext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	again()
}
