//go:build linux || openbsd || solaris || illumos || aix

package regularfile

import (
	"os"
	"syscall"
)

// ChangeTime is the file's status change time in nanoseconds, which, unlike
// its modification time, a user cannot set back.
func ChangeTime(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Nano()
	}
	return 0
}
