package app

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// folderID is a folder's device, inode and birth time: an inode alone is
// reused, so a folder made again at the path could pass for the old one.
// Without a birth time it is the device and inode; "" where unreadable.
func folderID(dir string) string {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, dir, 0, unix.STATX_INO|unix.STATX_BTIME, &st); err != nil {
		return ""
	}
	dev := unix.Mkdev(st.Dev_major, st.Dev_minor)
	if st.Mask&unix.STATX_BTIME == 0 {
		return fmt.Sprintf("%d:%d", dev, st.Ino)
	}
	return fmt.Sprintf("%d:%d:%d.%09d", dev, st.Ino, st.Btime.Sec, st.Btime.Nsec)
}
