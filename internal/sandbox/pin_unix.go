//go:build unix

package sandbox

import (
	"os"
	"syscall"
)

// openPinned opens the file at path to hold it: never through a link, and
// without waiting on a FIFO swapped in.
func openPinned(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0) // #nosec G304 -- the file the operator named, opened only to hold it
}
