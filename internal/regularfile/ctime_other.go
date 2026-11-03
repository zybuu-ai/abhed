//go:build !(linux || openbsd || dragonfly || darwin || freebsd || netbsd || windows)

package regularfile

import "os"

// ChangeTime is 0 where Abhed does not read it (systems it is not built for):
// the size, modification time and ID still say whether the file changed.
func ChangeTime(os.FileInfo) int64 { return 0 }
