//go:build !darwin && !linux

package sandbox

import "os"

// changeTime is a file's ctime where the platform gives it; here it does
// not, and size, mtime and mode are compared alone.
func changeTime(os.FileInfo) (int64, bool) { return 0, false }
