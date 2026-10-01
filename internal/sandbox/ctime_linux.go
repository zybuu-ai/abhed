package sandbox

import (
	"os"
	"syscall"
)

// changeTime is a file's ctime in nanoseconds, where the platform gives it.
func changeTime(fi os.FileInfo) (int64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Ctim.Nano(), true
}
