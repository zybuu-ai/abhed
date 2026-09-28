//go:build unix

package sandboxconfig

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// writable reports whether this user owns f or may write it.
func writable(f string, info os.FileInfo) bool {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Getuid() {
		return true
	}
	return unix.Access(f, unix.W_OK) == nil
}
