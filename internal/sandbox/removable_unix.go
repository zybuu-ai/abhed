//go:build unix

package sandbox

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// makeRemovable makes the folders under dir, which sandboxed commands wrote,
// owner-accessible for RemoveAll. It never follows a link, at dir or inside it
// (RemoveAll then removes the link itself), never changes a file, and has no
// depth limit a command could build past.
func makeRemovable(dir string) {
	if parent, err := filepath.EvalSymlinks(filepath.Dir(dir)); err == nil {
		dir = filepath.Join(parent, filepath.Base(dir))
	}
	parent, err := unix.Open(filepath.Dir(dir), dirSearchOpen|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer func() { _ = unix.Close(parent) }()
	top, ok := enterRemovable(parent, filepath.Base(dir))
	if !ok {
		return
	}
	// One descriptor is held, the deepest; a level is returned to by "..",
	// checked to be the folder left, so a moved folder ends the walk.
	stack := []removableLevel{top}
	for len(stack) > 0 {
		cur := &stack[len(stack)-1]
		if cur.next < len(cur.names) {
			name := cur.names[cur.next]
			cur.next++
			if child, ok := enterRemovable(cur.fd, name); ok {
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

// removableLevel is a folder of the walk: its descriptor while it is the
// deepest, its identity, and the names in it still to visit.
type removableLevel struct {
	fd    int
	st    unix.Stat_t
	names []string
	next  int
}

// enterRemovable makes the folder name in parent owner-accessible without
// following a link, opens it and lists it; anything but a folder is skipped.
func enterRemovable(parent int, name string) (removableLevel, bool) {
	var st unix.Stat_t
	if unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil || statMode(&st)&unix.S_IFMT != unix.S_IFDIR {
		return removableLevel{}, false
	}
	if perm := statMode(&st) & 0o7777; perm&0o700 != 0o700 {
		_ = chmodNoFollow(parent, name, perm|0o700)
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return removableLevel{}, false
	}
	// What was opened is a folder, never a link, swapped in or not.
	if unix.Fstat(fd, &st) != nil {
		_ = unix.Close(fd)
		return removableLevel{}, false
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
	return removableLevel{fd: fd, st: st, names: names}, true
}
