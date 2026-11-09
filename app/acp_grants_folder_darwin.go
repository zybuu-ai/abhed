package app

import (
	"fmt"
	"os"
	"syscall"
)

// folderID is a folder's device, inode and birth time: an inode alone is
// reused, so a folder made again at the path could pass for the old one.
func folderID(dir string) string {
	info, err := os.Stat(dir)
	if err != nil {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d.%09d", st.Dev, st.Ino, st.Birthtimespec.Sec, st.Birthtimespec.Nsec)
}
