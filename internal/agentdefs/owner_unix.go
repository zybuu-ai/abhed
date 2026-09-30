//go:build unix

package agentdefs

import (
	"os"
	"syscall"
)

// rootOwnedNotShared: owned by uid 0 and, but for a link (whose own mode
// means nothing), not group- or world-writable.
func rootOwnedNotShared(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 == 0
}
