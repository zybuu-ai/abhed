//go:build unix

package app

import (
	"os"

	"golang.org/x/sys/unix"
)

// stdinWaiting reports, without reading, whether stdin already holds input:
// a non-empty file, or a pipe with data in it.
func stdinWaiting() bool {
	if st, err := os.Stdin.Stat(); err == nil && st.Mode().IsRegular() {
		return st.Size() > 0
	}
	fds := []unix.PollFd{{Fd: int32(os.Stdin.Fd()), Events: unix.POLLIN}} // #nosec G115 -- a file descriptor fits in int32
	n, err := unix.Poll(fds, 0)
	return err == nil && n > 0 && fds[0].Revents&unix.POLLIN != 0
}
