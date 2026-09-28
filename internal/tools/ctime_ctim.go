//go:build linux || openbsd || dragonfly

package tools

import (
	"os"
	"syscall"
)

// changeTime is the inode's change time, which no user can set back, or 0.
func changeTime(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Nano()
	}
	return 0
}
