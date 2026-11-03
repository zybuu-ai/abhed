//go:build unix

package filelock

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openLock opens or makes the lock file without following a link at its
// name and without waiting on a pipe put there: it may sit in a folder a
// repository controls, such as a workspace's .abhed. It must be a plain file.
func openLock(lock string) (*os.File, error) {
	fd, err := unix.Open(lock, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), lock)
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: not a plain file", lock)
	}
	return f, nil
}

// tryLockFile takes an exclusive advisory lock on f without waiting, and
// reports whether it did. The kernel drops it when the holder exits.
func tryLockFile(f *os.File) (bool, error) {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.EWOULDBLOCK):
			return false, nil
		case !errors.Is(err, unix.EINTR):
			return false, err
		}
	}
}

func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
