//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package tools

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ripgrepWritable reports why the program at path is not run: it, or the
// folder holding it, is one this user, or anyone, can change. The agent's
// commands run as this user, so on a tier that does not keep them out of
// that folder (Homebrew's, for one) they could replace it.
func ripgrepWritable(path string) bool {
	for _, p := range []string{path, filepath.Dir(path)} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm()&0o022 != 0 || unix.Access(p, unix.W_OK) == nil {
			return true
		}
	}
	return false
}
