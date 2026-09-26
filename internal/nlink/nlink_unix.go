//go:build unix

package nlink

import (
	"os"
	"syscall"
)

// Of returns how many names the file has, or 1 when that cannot be read.
func Of(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) //nolint:unconvert // the field's type differs by platform
	}
	return 1
}
