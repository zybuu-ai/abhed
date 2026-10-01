//go:build unix

package ui

import (
	"os"
	"syscall"
)

// ownedByMe reports whether this process's user owns the file.
func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
