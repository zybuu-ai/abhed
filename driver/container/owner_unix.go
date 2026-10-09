//go:build unix

package container

import (
	"os"
	"syscall"
)

// owner is the uid and gid owning path, or -1 when unknown.
func owner(path string) (int, int) {
	info, err := os.Stat(path)
	if err != nil {
		return -1, -1
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1
}
