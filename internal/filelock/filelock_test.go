package filelock

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const busy = "%s is held (waited %s)"

// A held lock is not got again: the second taker waits its full wait, then
// fails with the busy text naming the lock and the wait.
func TestLockBusyTimesOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json.lock")
	unlock, err := Lock(path, time.Second, busy)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	wait := 150 * time.Millisecond
	start := time.Now()
	again, err := Lock(path, wait, busy)
	if err == nil {
		again()
		t.Fatal("a held lock was got a second time")
	}
	if took := time.Since(start); took < wait {
		t.Errorf("gave up after %s, before its wait of %s", took, wait)
	}
	if want := path + " is held (waited " + wait.String() + ")"; err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}
}

// A taker waiting on a held lock gets it once the holder lets go, and a
// released lock is free to take again.
func TestLockWaitsForTheHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Lock(path, time.Second, busy)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(released)
		unlock()
	}()
	next, err := Lock(path, 10*time.Second, busy)
	if err != nil {
		t.Fatalf("waiting taker: %v", err)
	}
	select {
	case <-released:
	default:
		t.Error("got the lock while it was still held")
	}
	next()
	last, err := Lock(path, 0, busy)
	if err != nil {
		t.Fatalf("a released lock: %v", err)
	}
	last()
}

// A lock file that cannot be made is an error, not a wait.
func TestLockUnopenable(t *testing.T) {
	_, err := Lock(filepath.Join(t.TempDir(), "missing", "x.lock"), time.Second, busy)
	if err == nil || strings.Contains(err.Error(), "is held") {
		t.Errorf("err %v", err)
	}
}
