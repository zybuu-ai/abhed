//go:build unix

package agentdefs

import (
	"os"
	"syscall"
)

// rootOwnedNotShared: owned by uid 0, and not group- or world-writable.
func rootOwnedNotShared(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0 && info.Mode().Perm()&0o022 == 0
}
