//go:build unix

package hostgit

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// untrustedFile says why config file f, where it exists, is not the
// person's alone: a hard link (another name may lie where commands write),
// owned by someone but the person or root, or it or its folder writable by
// others. "" when none, or when it is not there.
func untrustedFile(f string) string {
	real := sandbox.RealPath(f)
	info, err := os.Stat(real)
	if err != nil {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	me := uint32(os.Getuid()) // #nosec G115 -- a uid
	switch {
	case st.Nlink > 1:
		return "has another name (a hard link), which could lie where commands can write"
	case st.Uid != me && st.Uid != 0:
		return "is owned by another user"
	case info.Mode().Perm()&0o022 != 0:
		return "is writable by other users"
	}
	if d, err := os.Stat(filepath.Dir(real)); err == nil {
		dst, _ := d.Sys().(*syscall.Stat_t)
		switch {
		case dst != nil && dst.Uid != me && dst.Uid != 0:
			return "is in a folder owned by another user"
		case d.Mode().Perm()&0o022 != 0 && d.Mode()&os.ModeSticky == 0:
			return "is in a folder other users can write"
		}
	}
	return ""
}
