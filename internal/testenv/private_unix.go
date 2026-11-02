//go:build unix

package testenv

import (
	"os"
	"path/filepath"
	"syscall"
)

// privateFolder reports whether dir is a folder made as Isolate makes one:
// absolute, not a link, mode exactly 0700 and this user's.
func privateFolder(dir string) bool {
	if !filepath.IsAbs(dir) {
		return false
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm() != 0o700 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
