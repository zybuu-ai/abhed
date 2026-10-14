//go:build unix

package sandbox

import (
	"errors"
	"syscall"
)

// processExists asks the kernel whether pid is a process, by a null signal:
// one of another user's answers EPERM, and still exists.
func processExists(pid int) existence {
	switch err := syscall.Kill(pid, 0); {
	case err == nil, errors.Is(err, syscall.EPERM):
		return existsYes
	case errors.Is(err, syscall.ESRCH):
		return existsNo
	}
	return existsUnknown
}
