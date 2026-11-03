//go:build windows

package regularfile

import "os"

// Windows has no links a plain open follows unasked, nor pipes at a path.
func openNoFollow(path string) (*os.File, error) {
	return os.Open(path) // #nosec G304 -- checked a plain file first, and again once open
}

// ID is 0 on Windows: the size and time say whether the file changed.
func ID(os.FileInfo) uint64 { return 0 }
