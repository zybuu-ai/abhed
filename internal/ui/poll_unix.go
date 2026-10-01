//go:build unix

package ui

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// readyFunc reports, for a terminal, whether input arrives within d. It is how
// a lone Esc is told apart from an escape sequence split across reads, which
// happens over a slow SSH link, and how an Enter inside a paste is told from
// one a person pressed.
func readyFunc(f *os.File) func(time.Duration) bool {
	fd := int32(f.Fd()) // #nosec G115 -- a file descriptor fits in 32 bits
	return func(d time.Duration) bool {
		fds := []unix.PollFd{{Fd: fd, Events: unix.POLLIN}}
		for {
			n, err := unix.Poll(fds, int(d/time.Millisecond))
			if err == unix.EINTR {
				continue
			}
			return err == nil && n > 0
		}
	}
}
