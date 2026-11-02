//go:build unix

package app

import (
	"fmt"
	"os"
	"syscall"
)

// folderID is a folder's device and inode, which a folder made again at the
// same path does not share; "" where it cannot be read.
func folderID(dir string) string {
	info, err := os.Stat(dir)
	if err != nil {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}
