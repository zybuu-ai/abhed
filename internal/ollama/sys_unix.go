//go:build darwin || linux || freebsd || netbsd || openbsd

package ollama

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// diskFree is the bytes free to this user on the file system holding path,
// or its nearest folder that exists.
func diskFree(path string) (uint64, error) {
	p := existing(path)
	var fs unix.Statfs_t
	if err := unix.Statfs(p, &fs); err != nil {
		return 0, err
	}
	return uint64(fs.Bavail) * uint64(fs.Bsize), nil //nolint:unconvert // the field types differ by platform
}

// existing is path, or the nearest folder above it that exists.
func existing(path string) string {
	for p := path; ; {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		up := filepath.Dir(p)
		if up == p {
			return p
		}
		p = up
	}
}
