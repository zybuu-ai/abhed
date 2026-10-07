//go:build linux

package mountns

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Attr starts the process it is given to in a new user and mount namespace,
// as the same user and group, holding CAP_SYS_ADMIN there and nothing else,
// so it can apply a Plan before it drops it.
func Attr(a *syscall.SysProcAttr) {
	a.Cloneflags |= unix.CLONE_NEWUSER | unix.CLONE_NEWNS
	a.UidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}}
	a.GidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}}
	a.GidMappingsEnableSetgroups = false
	a.AmbientCaps = []uintptr{unix.CAP_SYS_ADMIN}
}

// Apply makes every mount private to this namespace, then applies p: the
// pins, the read-only binds and the empty folders, in that order, at the
// workspace and at every other mount that reaches its files (an alias), so
// no second path to them stays open. An alias it cannot cover refuses the
// plan. It needs CAP_SYS_ADMIN in the namespace, and checks each mount.
func Apply(p Plan) error { return apply(p, true) }

// apply is Apply, covering the aliases only when asked: the probe's self
// test checks the mechanisms in a temp folder, whose filesystem's other
// mounts say nothing about the workspace's.
func apply(p Plan, covered bool) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("making this namespace's mounts private: %w", err)
	}
	root, err := unix.Open(p.Root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", p.Root, err)
	}
	defer func() { _ = unix.Close(root) }()
	// Found and opened before any mount, which could hide one's path.
	var targets []aliasTarget
	if covered {
		if targets, _, err = findAliases(root); err != nil {
			return err
		}
	}
	defer closeAll(targets)
	if err := applyAt(root, p.Root, p); err != nil {
		return err
	}
	for _, t := range targets {
		if err := t.cover(p); err != nil {
			return fmt.Errorf("covering %s, where the workspace is also mounted: %w", t.path, err)
		}
	}
	return nil
}

// Aliases are the other mounts of the workspace at root, checked to reach it,
// and those behind a folder no command can search; one unchecked is the error.
func Aliases(root string) (found, unreachable []string, err error) {
	fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	targets, unreachable, err := findAliases(fd)
	if err != nil {
		return nil, nil, err
	}
	defer closeAll(targets)
	found = make([]string, 0, len(targets))
	for _, t := range targets {
		found = append(found, t.path)
	}
	return found, unreachable, nil
}

// applyAt applies p's lists beneath root, named name in errors.
func applyAt(root int, name string, p Plan) error {
	for _, rel := range p.Pin {
		if err := bindSelf(root, rel, false); err != nil {
			return fmt.Errorf("pinning %s: %w", filepath.Join(name, rel), err)
		}
	}
	for _, rel := range p.ReadOnly {
		if err := bindSelf(root, rel, true); err != nil {
			return fmt.Errorf("holding %s read-only: %w", filepath.Join(name, rel), err)
		}
	}
	for _, rel := range p.Empty {
		if err := empty(root, rel); err != nil {
			return fmt.Errorf("hiding %s: %w", filepath.Join(name, rel), err)
		}
	}
	return nil
}

// aliasTarget is an alias opened and checked to be the workspace, or the
// part of it rel names.
type aliasTarget struct {
	fd   int
	path string
	rel  string
}

func closeAll(ts []aliasTarget) {
	for _, t := range ts {
		_ = unix.Close(t.fd)
	}
}

// findAliases opens every other mount of the workspace's filesystem showing
// it, checked by identity; a path that reaches something else refuses.
func findAliases(root int) (_ []aliasTarget, unreachable []string, err error) {
	var stx unix.Statx_t
	if err := unix.Statx(root, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &stx); err != nil {
		return nil, nil, fmt.Errorf("finding the workspace's mount: %w", err)
	}
	if stx.Mask&unix.STATX_MNT_ID == 0 {
		return nil, nil, errors.New("this kernel does not say which mount the workspace is on, so its other mounts cannot be found")
	}
	wsPath, err := os.Readlink(fdPath(root))
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, nil, err
	}
	entries, err := parseMountinfo(string(data))
	if err != nil {
		return nil, nil, err
	}
	found, deleted, err := aliases(entries, int(stx.Mnt_id), wsPath)
	if err != nil {
		return nil, nil, err
	}
	if len(found) == 0 && len(deleted) == 0 {
		return nil, nil, nil
	}
	slash, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = unix.Close(slash) }()
	var out []aliasTarget
	defer func() {
		if err != nil {
			closeAll(out)
		}
	}()
	for _, point := range deleted {
		if err := deadMount(slash, point); err != nil {
			return nil, nil, fmt.Errorf("%w: %s: %w", errAlias, point, err)
		}
	}
	for _, a := range found {
		var want unix.Stat_t
		if err := statBeneath(root, a.Rel, &want); err != nil {
			return nil, nil, fmt.Errorf("%w: %s shows %s of the workspace, which cannot be checked: %w", errAlias, a.Path, a.Rel, err)
		}
		fd, err := beneath(slash, fromSlash(a.Path), 0)
		if errors.Is(err, unix.EACCES) && barred(slash, a.Path) {
			unreachable = append(unreachable, a.Path)
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%w: the workspace is also mounted at %s, which cannot be opened to cover it: %w", errAlias, a.Path, err)
		}
		var got unix.Stat_t
		if err := unix.Fstat(fd, &got); err != nil || got.Dev != want.Dev || got.Ino != want.Ino {
			_ = unix.Close(fd)
			return nil, nil, fmt.Errorf("%w: the workspace is also mounted at %s, and that path does not reach it now, so it cannot be covered", errAlias, a.Path)
		}
		out = append(out, aliasTarget{fd: fd, path: a.Path, rel: a.Rel})
	}
	return out, unreachable, nil
}

// barred is whether a folder on the way to p refuses this user search and is
// not this user's, so a command, as this user, can neither pass nor chmod it.
func barred(slash int, p string) bool {
	cur, err := unix.Dup(slash)
	if err != nil {
		return false
	}
	defer func() { _ = unix.Close(cur) }()
	for _, c := range strings.Split(fromSlash(p), "/") {
		next, err := beneath(cur, c, 0)
		if errors.Is(err, unix.EACCES) {
			var st unix.Stat_t
			return unix.Fstat(cur, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR &&
				int64(st.Uid) != int64(os.Geteuid())
		}
		if err != nil {
			return false
		}
		_ = unix.Close(cur)
		cur = next
	}
	return false
}

// statBeneath stats rel beneath root, or root itself when rel is "".
func statBeneath(root int, rel string, st *unix.Stat_t) error {
	if rel == "" {
		return unix.Fstat(root, st)
	}
	fd, err := beneath(root, rel, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return unix.Fstat(fd, st)
}

// deadMount checks a mount whose root was unlinked is a removed folder or a
// file with no name left; a file with another name may be the workspace's.
func deadMount(slash int, point string) error {
	var st unix.Stat_t
	if err := statBeneath(slash, fromSlash(point), &st); err != nil {
		return fmt.Errorf("a mount of the workspace's filesystem whose root was removed cannot be checked: %w", err)
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR || st.Nlink == 0 {
		return nil
	}
	return errors.New("a mount of the workspace's filesystem shows a removed name of a file that still has another, which may be in the workspace")
}

// fromSlash is an absolute path as a path beneath "/".
func fromSlash(p string) string {
	if r := strings.TrimPrefix(p, "/"); r != "" {
		return r
	}
	return "."
}

// cover applies p at the alias: all of it where the alias shows the whole
// workspace, else the part inside what it shows, or covers the alias whole
// where it shows part of a folder p hides or holds read-only.
func (t aliasTarget) cover(p Plan) error {
	if t.rel == "" {
		return applyAt(t.fd, t.path, p)
	}
	sub, whole := rebase(p, t.rel)
	slash, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(slash) }()
	reopen := func() (int, error) { return beneath(slash, fromSlash(t.path), 0) }
	switch whole {
	case wholeReadOnly:
		return bindFD(t.fd, reopen, true)
	case wholeEmpty:
		var st unix.Stat_t
		if err := unix.Fstat(t.fd, &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return errors.New("it shows a file the fence hides, and only a folder can be covered")
		}
		return emptyFD(t.fd, reopen)
	}
	if sub.isEmpty() {
		return nil
	}
	return applyAt(t.fd, t.path, sub)
}

// isEmpty is whether a plan holds nothing to apply.
func (p Plan) isEmpty() bool { return len(p.Pin)+len(p.ReadOnly)+len(p.Empty) == 0 }

// beneath opens rel under root by path alone, refusing a symbolic link at
// any step and any escape from root; it crosses mounts, so a path opened
// again after a mount reaches the mount.
func beneath(root int, rel string, flags uint64) (int, error) {
	return unix.Openat2(root, rel, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC | unix.O_NOFOLLOW | flags,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
}

func fdPath(fd int) string { return "/proc/self/fd/" + strconv.Itoa(fd) }

// bindSelf binds rel onto itself through its descriptor, and makes the new
// mount read-only when ro (see bindFD).
func bindSelf(root int, rel string, ro bool) error {
	fd, err := beneath(root, rel, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return bindFD(fd, func() (int, error) { return beneath(root, rel, 0) }, ro)
}

// bindFD binds fd onto itself, read-only when ro; reopen reaches the new mount.
// A file with a name outside the bind is refused: it would stay writable.
func bindFD(fd int, reopen func() (int, error), ro bool) error {
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return err
	}
	if t := before.Mode & unix.S_IFMT; t != unix.S_IFDIR && t != unix.S_IFREG {
		return errors.New("neither a folder nor a file")
	}
	if before.Mode&unix.S_IFMT == unix.S_IFREG && before.Nlink > 1 {
		return fmt.Errorf("the file has %d names (hard links), and would stay writable through another", before.Nlink)
	}
	if err := unix.Mount(fdPath(fd), fdPath(fd), "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return err
	}
	if !ro {
		return nil
	}
	// Opened again, the path reaches the mount just made.
	top, err := reopen()
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(top) }()
	var after unix.Stat_t
	if err := unix.Fstat(top, &after); err != nil {
		return err
	}
	if after.Dev != before.Dev || after.Ino != before.Ino {
		return errors.New("the path changed while it was being bound")
	}
	var st unix.Statfs_t
	if err := unix.Fstatfs(top, &st); err != nil {
		return err
	}
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY)
	flags |= locked(int64(st.Flags)) //nolint:unconvert // Flags is uint32 on s390x
	if err := unix.Mount("", fdPath(top), "", flags, ""); err != nil {
		return fmt.Errorf("remounting read-only: %w", err)
	}
	if err := unix.Fstatfs(top, &st); err != nil {
		return err
	}
	if st.Flags&unix.ST_RDONLY == 0 {
		return errors.New("the mount is not read-only after remounting it")
	}
	if after.Mode&unix.S_IFMT == unix.S_IFDIR {
		// Read-only now, so no link out of it can be made after the walk.
		return heldAlone(top, after.Dev)
	}
	return nil
}

// heldWalkEntries bounds the walk of a folder held read-only; one larger is
// refused rather than held unchecked.
const heldWalkEntries = 20000

// heldWalkBatch is how many names heldAlone reads from a folder at a time.
const heldWalkBatch = 512

// heldAlone refuses a file in the read-only folder dir with a name outside it,
// or a mount inside it, either of which would stay writable.
func heldAlone(dir int, dev uint64) error {
	type file struct {
		name         string
		nlink, names uint64
	}
	files := map[[2]uint64]*file{}
	seen := 0
	var walk func(fd int, at string, depth int) error
	var visit func(d int, at string, depth int, names []string) error
	walk = func(fd int, at string, depth int) error {
		if depth > 64 {
			return errors.New("it is deeper than 64 folders to check for files named elsewhere")
		}
		d, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(d), at) // #nosec G115 -- a descriptor fits
		defer func() { _ = f.Close() }()
		for {
			// Read in batches, so a huge folder is refused before it is all in memory.
			names, err := f.Readdirnames(heldWalkBatch)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if seen += len(names); seen > heldWalkEntries {
				return fmt.Errorf("it holds more than %d entries to check for files named elsewhere", heldWalkEntries)
			}
			if err := visit(d, at, depth, names); err != nil {
				return err
			}
		}
	}
	visit = func(d int, at string, depth int, names []string) error {
		for _, n := range names {
			var st unix.Stat_t
			if err := unix.Fstatat(d, n, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return err
			}
			p := filepath.Join(at, n)
			if st.Dev != dev {
				return fmt.Errorf("%s is another mount, which would stay writable", p)
			}
			switch st.Mode & unix.S_IFMT {
			case unix.S_IFREG:
				k := [2]uint64{st.Dev, st.Ino}
				if files[k] == nil {
					files[k] = &file{name: p, nlink: uint64(st.Nlink)} //nolint:unconvert // Nlink is uint32 on arm64 and s390x
				}
				files[k].names++
			case unix.S_IFDIR:
				sub, err := unix.Openat(d, n, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				if err != nil {
					return err
				}
				err = walk(sub, p, depth+1)
				_ = unix.Close(sub)
				if err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(dir, ".", 0); err != nil {
		return err
	}
	for _, f := range files {
		if f.names < f.nlink {
			return fmt.Errorf("%s in it has a name (a hard link) outside it, and would stay writable through that", f.name)
		}
	}
	return nil
}

// locked are the flags of the mount below that a namespace's remount must
// keep: an ordinary user may not clear them.
func locked(f int64) uintptr {
	var out uintptr
	for st, ms := range map[int64]uintptr{
		unix.ST_NOSUID: unix.MS_NOSUID, unix.ST_NODEV: unix.MS_NODEV, unix.ST_NOEXEC: unix.MS_NOEXEC,
		unix.ST_NOATIME: unix.MS_NOATIME, unix.ST_NODIRATIME: unix.MS_NODIRATIME, unix.ST_RELATIME: unix.MS_RELATIME,
	} {
		if f&st != 0 {
			out |= ms
		}
	}
	return out
}

// empty covers the folder rel with an empty tmpfs, gone when the namespace is.
func empty(root int, rel string) error {
	fd, err := beneath(root, rel, unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return emptyFD(fd, func() (int, error) { return beneath(root, rel, unix.O_DIRECTORY) })
}

// emptyFD covers the folder fd with an empty tmpfs; reopen opens its path
// again, reaching the tmpfs.
func emptyFD(fd int, reopen func() (int, error)) error {
	if err := unix.Mount("tmpfs", fdPath(fd), "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0700,size=16m"); err != nil {
		return err
	}
	top, err := reopen()
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(top) }()
	var st unix.Statfs_t
	if err := unix.Fstatfs(top, &st); err != nil {
		return err
	}
	if st.Type != unix.TMPFS_MAGIC {
		return errors.New("the folder is not covered after mounting over it")
	}
	return nil
}

// Drop clears this thread's capabilities, ambient, inheritable, permitted and
// effective, and checks none is left. The command is executed from this
// thread, so it starts with none.
func Drop() error {
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("clearing the ambient capabilities: %w", err)
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("dropping the capabilities: %w", err)
	}
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return fmt.Errorf("reading the capabilities back: %w", err)
	}
	for _, d := range data {
		if d.Effective|d.Permitted|d.Inheritable != 0 {
			return errors.New("a capability is left after dropping them")
		}
	}
	return nil
}

// SelfTest applies a plan over dir, which holds the folders ro, pin and
// hidden (with a file secret), as a launcher would; checks a write to ro
// is refused and secret is hidden; and drops the capabilities. It runs in a
// process started with Attr, and says what it saw.
func SelfTest(dir string) (string, error) {
	p := Plan{Root: dir, Pin: []string{"pin"}, ReadOnly: []string{"ro"}, Empty: []string{"hidden"}}
	if err := apply(p, false); err != nil {
		return "", err
	}
	err := os.WriteFile(filepath.Join(dir, "ro", "x"), nil, 0o600)
	if !errors.Is(err, unix.EROFS) {
		return "", fmt.Errorf("a write under the read-only bind was not refused as read-only (%w)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hidden", "secret")); !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("a file under the empty folder is still visible (%w)", err)
	}
	if err := unix.Rename(filepath.Join(dir, "pin"), filepath.Join(dir, "moved")); !errors.Is(err, unix.EBUSY) {
		return "", fmt.Errorf("a pinned folder was not kept in place (%w)", err)
	}
	if err := Drop(); err != nil {
		return "", err
	}
	return "read-only bind, tmpfs and pin applied in a user namespace; capabilities dropped", nil
}
