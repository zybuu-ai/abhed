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

// openFileNoFollow opens the file name in parent, never through a link nor
// waiting on a FIFO; one its owner can neither read nor write is left, as
// no command is fenced here.
func openFileNoFollow(parent int, name string) (int, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		fd, err = unix.Openat(parent, name, unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	}
	return fd, err
}

// chmodHeld changes the mode of the file fd holds.
func chmodHeld(fd int, mode uint32) error { return unix.Fchmod(fd, mode) }

// statMode is st's mode, which is narrower here than on Linux.
func statMode(st *unix.Stat_t) uint32 { return uint32(st.Mode) }
