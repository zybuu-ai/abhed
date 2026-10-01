//go:build unix

package secrets

import (
	"os"
	"syscall"
)

// openStore opens without blocking, so a FIFO or device at the path is opened
// and then refused on what fstat says, never waited on.
func openStore(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
