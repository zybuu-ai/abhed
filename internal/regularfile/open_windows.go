//go:build windows

package regularfile

import "os"

// openNoFollow opens path. On Windows the open follows a link or junction,
// but Open has already refused a link at the path, and a link swapped in
// after that look leads to a different file, which the same-file check
// refuses.
func openNoFollow(path string) (*os.File, error) {
	return os.Open(path) // #nosec G304 -- checked a plain file first, and again once open
}

// ID is 0 on Windows: the size and times say whether the file changed.
func ID(os.FileInfo) uint64 { return 0 }

// ChangeTime is 0 on Windows, where FileInfo does not carry it.
func ChangeTime(os.FileInfo) int64 { return 0 }
