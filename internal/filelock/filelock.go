// Package filelock holds an exclusive advisory lock on a file beside the one
// it guards, across processes. The kernel drops it when the holder exits, so
// a crash leaves nothing held.
package filelock

import (
	"fmt"
	"os"
	"time"
)

// Lock holds an exclusive lock on lock until the returned function runs,
// waiting at most wait. busy is the error's text when it is not got in time,
// formatted with the lock's path and the wait.
func Lock(lock string, wait time.Duration, busy string) (func(), error) {
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- a lock file beside the caller's own file
	if err != nil {
		return nil, err
	}
	for deadline := time.Now().Add(wait); ; time.Sleep(20 * time.Millisecond) {
		ok, err := tryLockFile(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", lock, err)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf(busy, lock, wait)
		}
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}
