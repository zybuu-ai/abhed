//go:build linux

package mountns

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
// pins, the read-only binds, the empty folders and the null files, in that
// order. It needs CAP_SYS_ADMIN in the namespace, and checks each mount.
func Apply(p Plan) error {
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
	for _, rel := range p.Pin {
		if err := bindSelf(root, rel, false); err != nil {
			return fmt.Errorf("pinning %s: %w", filepath.Join(p.Root, rel), err)
		}
	}
	for _, rel := range p.ReadOnly {
		if err := bindSelf(root, rel, true); err != nil {
			return fmt.Errorf("holding %s read-only: %w", filepath.Join(p.Root, rel), err)
		}
	}
	for _, rel := range p.Empty {
		if err := empty(root, rel); err != nil {
			return fmt.Errorf("hiding %s: %w", filepath.Join(p.Root, rel), err)
		}
	}
	for _, rel := range p.Null {
		if err := null(root, rel); err != nil {
			return fmt.Errorf("hiding %s: %w", filepath.Join(p.Root, rel), err)
		}
	}
	return nil
}

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
// mount read-only when ro, keeping the flags the mount below it is locked to.
func bindSelf(root int, rel string, ro bool) error {
	fd, err := beneath(root, rel, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return err
	}
	if t := before.Mode & unix.S_IFMT; t != unix.S_IFDIR && t != unix.S_IFREG {
		return errors.New("neither a folder nor a file")
	}
	if err := unix.Mount(fdPath(fd), fdPath(fd), "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return err
	}
	if !ro {
		return nil
	}
	// Opened again, the path reaches the mount just made.
	top, err := beneath(root, rel, 0)
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
	flags |= locked(st.Flags)
	if err := unix.Mount("", fdPath(top), "", flags, ""); err != nil {
		return fmt.Errorf("remounting read-only: %w", err)
	}
	if err := unix.Fstatfs(top, &st); err != nil {
		return err
	}
	if st.Flags&unix.ST_RDONLY == 0 {
		return errors.New("the mount is not read-only after remounting it")
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
	if err := unix.Mount("tmpfs", fdPath(fd), "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0700,size=16m"); err != nil {
		return err
	}
	top, err := beneath(root, rel, unix.O_DIRECTORY)
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

// null covers the file rel with /dev/null.
func null(root int, rel string) error {
	fd, err := beneath(root, rel, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		return errors.New("a folder, not a file")
	}
	return unix.Mount("/dev/null", fdPath(fd), "", unix.MS_BIND, "")
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
	if err := Apply(p); err != nil {
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
