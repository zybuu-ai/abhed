//go:build !(darwin || freebsd || netbsd || linux || openbsd || dragonfly)

package tools

import "os"

// changeTime is 0: the change time is not read on this platform, so the modification time decides.
func changeTime(os.FileInfo) int64 { return 0 }
