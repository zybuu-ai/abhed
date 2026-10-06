//go:build unix

package sandbox

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ownerAccessDepth bounds the walk, which holds a descriptor per level.
const ownerAccessDepth = 128

// restoreOwnerAccess gives the owner read, write and search on the folder
// at p and every folder under it, and read and write on its files, so a
// planted entry a command locked can be moved, read and removed. It never
// follows a link: each entry is reached relative to its parent's open
// descriptor, and anything that is not a plain folder or file is left alone.
func restoreOwnerAccess(p string) {
	parent, err := unix.Open(filepath.Dir(p), dirSearchOpen|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer func() { _ = unix.Close(parent) }()
	restoreOwnerAccessAt(parent, filepath.Base(p), 0)
}

func restoreOwnerAccessAt(parent int, name string, depth int) {
	var st unix.Stat_t
	if unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return
	}
	perm := statMode(&st) & 0o7777
	switch statMode(&st) & unix.S_IFMT {
	case unix.S_IFREG:
		// A hard link may share its inode with state made before the session.
		if perm&0o600 != 0o600 && st.Nlink <= 1 {
			_ = chmodNoFollow(parent, name, perm|0o600)
		}
		return
	case unix.S_IFDIR:
	default:
		return
	}
	// A folder without read cannot be opened to fchmod, so it is changed by
	// name first, without following a link there.
	if perm&0o700 != 0o700 {
		_ = chmodNoFollow(parent, name, perm|0o700)
	}
	if depth >= ownerAccessDepth {
		return
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	dir := os.NewFile(uintptr(fd), name) // #nosec G115 -- a descriptor fits
	defer func() { _ = dir.Close() }()
	// What was opened is a folder, never a link, swapped in or not.
	if unix.Fstat(fd, &st) == nil && statMode(&st)&0o700 != 0o700 {
		_ = unix.Fchmod(fd, (statMode(&st)&0o7777)|0o700)
	}
	names, _ := dir.Readdirnames(-1)
	for _, n := range names {
		restoreOwnerAccessAt(fd, n, depth+1)
	}
}
