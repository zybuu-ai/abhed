//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package tools

import (
	"os"
	"syscall"
)

// openDir opens only a folder, and never waits: a folder swapped for a FIFO is refused, not blocked on.
func openDir(dir string) (*os.File, error) {
	return os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0) // #nosec G304 -- lists a folder on the path being judged; no file is read
}
