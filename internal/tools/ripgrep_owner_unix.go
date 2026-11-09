//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package tools

import (
	"os"
	"path/filepath"
	"syscall"
)

// ripgrepWritable reports that the program at path is not run: it, or a
// folder anywhere above it, is not owned by root, or can be written by its
// group or by anyone. Only then can no account but root replace it, nor
// rename a folder on the way and put another program at the same path.
func ripgrepWritable(path string) bool {
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Stat(p)
		if err != nil {
			return true
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			return true
		}
		if filepath.Dir(p) == p {
			return false
		}
	}
}
