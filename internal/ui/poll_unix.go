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

// waitFunc waits, with no timeout, for input on f or for wake. wait reports
// true for input (or once the wake pipe is gone), false for a wake; stop
// ends the pipe. An idle session sleeps in poll rather than waking to look.
func waitFunc(f *os.File) (wait func() bool, wake, stop func()) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, nil
	}
	in, wfd := int32(f.Fd()), int32(r.Fd()) // #nosec G115 -- file descriptors fit in 32 bits
	dead := false
	buf := make([]byte, 64)
	wait = func() bool {
		if dead {
			return true
		}
		fds := []unix.PollFd{{Fd: in, Events: unix.POLLIN}, {Fd: wfd, Events: unix.POLLIN}}
		for {
			_, err := unix.Poll(fds, -1)
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				dead = true
				return true
			}
			break
		}
		if fds[0].Revents != 0 {
			return true
		}
		if n, err := r.Read(buf); err != nil || n == 0 {
			dead = true
			_ = r.Close()
			return true
		}
		return false
	}
	return wait, func() { _, _ = w.Write([]byte{0}) }, func() { _ = w.Close() }
}
