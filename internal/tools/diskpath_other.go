//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly)

package tools

import "os"

// openDir opens a folder; this platform has no flag to refuse a FIFO swapped in for it.
func openDir(dir string) (*os.File, error) {
	return os.Open(dir) // #nosec G304 -- lists a folder on the path being judged; no file is read
}
