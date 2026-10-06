//go:build unix && !linux

package sandbox

import "golang.org/x/sys/unix"

// dirSearchOpen opens the planted entry's parent folder to walk from.
const dirSearchOpen = unix.O_RDONLY

// chmodNoFollow changes the mode of name in the folder parent, never
// following a link there.
func chmodNoFollow(parent int, name string, mode uint32) error {
	return unix.Fchmodat(parent, name, mode, unix.AT_SYMLINK_NOFOLLOW)
}

// statMode is st's mode, which is narrower here than on Linux.
func statMode(st *unix.Stat_t) uint32 { return uint32(st.Mode) }
