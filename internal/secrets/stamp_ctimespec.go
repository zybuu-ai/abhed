//go:build darwin || freebsd || netbsd

package secrets

import (
	"os"
	"syscall"
)

// fileIdentity is the file's inode and change time, which no user can set
// back, or zeros where they cannot be read.
func fileIdentity(info os.FileInfo) (inode uint64, ctime int64) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ino, st.Ctimespec.Nano()
	}
	return 0, 0
}
