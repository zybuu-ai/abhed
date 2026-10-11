//go:build unix

package secrets

import (
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
