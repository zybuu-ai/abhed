//go:build unix

package testenv

import (
	"os"
	"syscall"
)

// privateDir reports whether dir belongs to this user and no one else may
// write in it, so nobody else can make a folder inside.
func privateDir(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o022 != 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
