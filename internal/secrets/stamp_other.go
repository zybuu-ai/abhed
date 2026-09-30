//go:build !(darwin || freebsd || netbsd || linux || openbsd || dragonfly)

package secrets

import "os"

// fileIdentity is not read on this platform; the modification time and size decide.
func fileIdentity(os.FileInfo) (inode uint64, ctime int64) { return 0, 0 }
