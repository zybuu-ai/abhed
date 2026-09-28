//go:build !unix

package sandboxconfig

import "os"

// writable reports whether this user may write f; without owners to read, any
// file that is not read-only counts.
func writable(_ string, info os.FileInfo) bool { return info.Mode().Perm()&0o200 != 0 }
