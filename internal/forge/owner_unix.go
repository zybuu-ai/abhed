//go:build unix

package forge

import (
	"os"
	"syscall"
)

// fileOwnedByMe reports whether the file belongs to the current user.
func fileOwnedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
