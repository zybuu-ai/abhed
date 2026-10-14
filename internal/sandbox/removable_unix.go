//go:build unix

package sandbox

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// makeRemovable makes the folders under dir, which sandboxed commands wrote,
// owner-accessible for RemoveAll. It never follows a link, at dir or inside it
// (RemoveAll then removes the link itself), and never changes a file.
func makeRemovable(dir string) {
	if parent, err := filepath.EvalSymlinks(filepath.Dir(dir)); err == nil {
		dir = filepath.Join(parent, filepath.Base(dir))
	}
	ownerAccessWalk(dir, false)
}

// restoreOwnerAccess makes p's tree owner-accessible so a locked planted entry
// can be moved, read and removed: its folders, and its files with one link.
// It never follows a link.
func restoreOwnerAccess(p string) { ownerAccessWalk(p, true) }

// ownerAccessWalk gives p's folders, and with files its files, back to their
// owner. It has no depth limit a command could build past: one descriptor is
// held, the deepest, and a level is returned to by "..", checked to be the
// folder left, so a folder moved mid-walk ends it.
func ownerAccessWalk(p string, files bool) {
	parent, err := unix.Open(filepath.Dir(p), dirSearchOpen|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer func() { _ = unix.Close(parent) }()
	top, ok := enterOwnerAccess(parent, filepath.Base(p), files)
	if !ok {
		return
	}
	stack := []ownerAccessLevel{top}
	for len(stack) > 0 {
		cur := &stack[len(stack)-1]
		if cur.next < len(cur.names) {
			name := cur.names[cur.next]
			cur.next++
			if child, ok := enterOwnerAccess(cur.fd, name, files); ok {
				_ = unix.Close(cur.fd)
				cur.fd = -1
				stack = append(stack, child)
			}
			continue
		}
		fd := cur.fd
		stack = stack[:len(stack)-1]
		if len(stack) == 0 {
			_ = unix.Close(fd)
			return
		}
		up := &stack[len(stack)-1]
		pfd, err := unix.Openat(fd, "..", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return
		}
		var st unix.Stat_t
		if unix.Fstat(pfd, &st) != nil || st.Dev != up.st.Dev || st.Ino != up.st.Ino {
			_ = unix.Close(pfd)
			return
		}
		up.fd = pfd
	}
}

// ownerAccessLevel is a folder of the walk: its descriptor while it is the
// deepest, its identity, and the names in it still to visit.
type ownerAccessLevel struct {
	fd    int
	st    unix.Stat_t
	names []string
	next  int
}

// enterOwnerAccess makes the folder name in parent owner-accessible without
// following a link, opens it and lists it. A file is given back when files
// is set, and like anything but a folder is not entered.
func enterOwnerAccess(parent int, name string, files bool) (ownerAccessLevel, bool) {
	var st unix.Stat_t
	if unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return ownerAccessLevel{}, false
	}
	switch statMode(&st) & unix.S_IFMT {
	case unix.S_IFDIR:
	case unix.S_IFREG:
		if files {
			restoreFileAccess(parent, name)
		}
		return ownerAccessLevel{}, false
	default:
		return ownerAccessLevel{}, false
	}
	if perm := statMode(&st) & 0o7777; perm&0o700 != 0o700 {
		_ = chmodNoFollow(parent, name, perm|0o700)
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ownerAccessLevel{}, false
	}
	// What was opened is a folder, never a link, swapped in or not.
	if unix.Fstat(fd, &st) != nil {
		_ = unix.Close(fd)
		return ownerAccessLevel{}, false
	}
	if statMode(&st)&0o700 != 0o700 {
		_ = unix.Fchmod(fd, (statMode(&st)&0o7777)|0o700)
	}
	// Listed through a copy of the descriptor, which closing the list leaves open.
	var names []string
	if dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0); err == nil { // #nosec G115 -- a descriptor fits
		f := os.NewFile(uintptr(dup), name) // #nosec G115 -- a descriptor fits
		names, _ = f.Readdirnames(-1)
		_ = f.Close()
	}
	return ownerAccessLevel{fd: fd, st: st, names: names}, true
}

// restoreFileAccess gives the file name in parent read and write for its
// owner, through a descriptor opened without following a link and checked
// there, so a file swapped in after the parent was listed is the one judged.
// A file with more than one link may share its inode with one outside.
func restoreFileAccess(parent int, name string) {
	fd, err := openFileNoFollow(parent, name)
	if err != nil {
		return
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || statMode(&st)&unix.S_IFMT != unix.S_IFREG || st.Nlink > 1 {
		return
	}
	if perm := statMode(&st) & 0o7777; perm&0o600 != 0o600 {
		_ = chmodHeld(fd, perm|0o600)
	}
}
