//go:build !windows

package microvm

import (
	"archive/tar"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/zybuu-ai/abhed/driver/microvm/guest"
)

// Bounds on one sync.
const (
	maxSyncEntries = 1 << 20
	maxNameLen     = 4096
)

// applier writes a guest's workspace changes into the host workspace. The
// guest is untrusted, so every path is checked before it is used, and every
// step goes through a folder already opened without following links:
// nothing is written outside the workspace, through a link, onto another
// filesystem, into Abhed's state or onto a write-protected path. A file is
// replaced by renaming a new one over it, never written in place, so a hard
// link to it elsewhere keeps its content.
type applier struct {
	rootFD int
	dev    uint64
	// owner, when set, owns everything created, and every folder changed
	// must already be theirs: a driver running as root writes only where
	// the workspace's user could.
	owner *Credential
	guard guard
	max   int64

	written int64
	entries int
	refused []string
}

func openApplier(root string, owner *Credential, protected []string, maxBytes int64) (*applier, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening the workspace %s: %w", root, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if owner != nil && st.Uid != owner.UID {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("the workspace %s is not owned by uid %d", root, owner.UID)
	}
	return &applier{rootFD: fd, dev: uint64(st.Dev), owner: owner, guard: newGuard(root, protected), max: maxBytes}, nil //nolint:unconvert // Dev is int32 on darwin
}

func (a *applier) close() { _ = unix.Close(a.rootFD) }

func (a *applier) refuse(rel, why string) {
	a.refused = append(a.refused, rel+": "+why)
}

// errSkip marks an entry refused and recorded; the sync goes on.
var errSkip = errors.New("skipped")

// apply takes one change. It returns an error only when the sync as a
// whole must stop; a refused entry is recorded and skipped.
func (a *applier) apply(ch guest.Change) error {
	a.entries++
	if a.entries > maxSyncEntries {
		return fmt.Errorf("the guest sent more than %d changes", maxSyncEntries)
	}
	name := ch.Path
	if ch.Header != nil {
		name = strings.TrimSuffix(ch.Header.Name, "/")
	}
	comps, err := cleanRel(name)
	if err != nil {
		a.refuse(fmt.Sprintf("%q", name), err.Error())
		return nil
	}
	rel := strings.Join(comps, "/")
	if why := a.guard.refuses(comps); why != "" {
		a.refuse(rel, why)
		return nil
	}
	switch {
	case ch.Deleted:
		err = a.remove(comps)
	case ch.Header.Typeflag == tar.TypeDir:
		err = a.mkdir(comps, ch.Header.Mode)
	case ch.Header.Typeflag == tar.TypeReg:
		err = a.file(comps, ch.Header, ch.Body)
	case ch.Header.Typeflag == tar.TypeSymlink:
		err = a.symlink(comps, ch.Header.Linkname)
	case ch.Header.Typeflag == tar.TypeLink:
		err = a.skip(rel, "hard links are not copied back")
	default:
		err = a.skip(rel, "only files, folders and symlinks are copied back")
	}
	if errors.Is(err, errSkip) {
		return nil
	}
	if err != nil && a.written <= a.max {
		a.refuse(rel, err.Error())
		return nil
	}
	return err
}

func (a *applier) skip(rel, why string) error {
	a.refuse(rel, why)
	return errSkip
}

// cleanRel splits a guest path into its parts, refusing anything but a
// plain relative path below the workspace.
func cleanRel(name string) ([]string, error) {
	switch {
	case name == "" || name == ".":
		return nil, errors.New("an empty path")
	case len(name) > maxNameLen:
		return nil, errors.New("a path too long")
	case strings.ContainsRune(name, 0):
		return nil, errors.New("a path with a NUL")
	case path.IsAbs(name):
		return nil, errors.New("an absolute path")
	case path.Clean(name) != name:
		return nil, errors.New("a path that is not in plain form")
	}
	comps := strings.Split(name, "/")
	for _, c := range comps {
		if c == ".." || c == "." || c == "" || len(c) > 255 {
			return nil, errors.New("a path that leaves the workspace")
		}
	}
	return comps, nil
}

// guard says which workspace paths are never written from the guest.
type guard struct{ protected [][]string }

func newGuard(root string, protected []string) guard {
	var g guard
	for _, p := range protected {
		if filepath.IsAbs(p) {
			rel, err := filepath.Rel(root, p)
			if err != nil || !filepath.IsLocal(rel) {
				continue
			}
			p = rel
		}
		if comps, err := cleanRel(filepath.ToSlash(filepath.Clean(p))); err == nil {
			g.protected = append(g.protected, comps)
		}
	}
	return g
}

// gitPointers are what git reads in a git folder as configuration, runs
// programs from, or follows elsewhere.
var gitPointerNames = []string{"config", "config.worktree", "commondir", "gitdir"}

// refuses says why comps may not be written or deleted, or "". Names are
// compared without case, as case-insensitive filesystems compare them.
func (g guard) refuses(comps []string) string {
	for i, c := range comps {
		if strings.EqualFold(c, ".abhed") {
			return "Abhed's state is never written from a sandbox"
		}
		if !strings.EqualFold(c, ".git") {
			continue
		}
		after := comps[i+1:]
		if len(after) == 0 {
			continue
		}
		for _, x := range after {
			if strings.EqualFold(x, "hooks") {
				return "git hooks are write-protected"
			}
		}
		last := after[len(after)-1]
		for _, n := range gitPointerNames {
			if strings.EqualFold(last, n) {
				return "git configuration is write-protected"
			}
		}
		if hasSuffixFold(after, "info", "attributes") || hasSuffixFold(after, "objects", "info", "alternates") {
			return "git configuration is write-protected"
		}
	}
	for _, p := range g.protected {
		if len(comps) >= len(p) && prefixFold(comps, p) {
			return "write-protected"
		}
	}
	return ""
}

func prefixFold(a, prefix []string) bool {
	for i := range prefix {
		if !strings.EqualFold(a[i], prefix[i]) {
			return false
		}
	}
	return true
}

func hasSuffixFold(a []string, suffix ...string) bool {
	return len(a) >= len(suffix) && prefixFold(a[len(a)-len(suffix):], suffix)
}

// isGitFile is a .git that is not a folder: it points git at another one.
func isGitFile(comps []string) bool { return strings.EqualFold(comps[len(comps)-1], ".git") }

// parent opens the folder holding comps' last part, making missing folders
// when create is set. No link is followed and no other filesystem entered.
func (a *applier) parent(comps []string, create bool) (int, error) {
	fd, err := unix.Dup(a.rootFD)
	if err != nil {
		return -1, err
	}
	for i, c := range comps[:len(comps)-1] {
		nfd, err := unix.Openat(fd, c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if why := a.guard.refuses(comps[:i+1]); why != "" {
				_ = unix.Close(fd)
				return -1, errors.New(why)
			}
			if err = a.mine(fd); err == nil {
				if err = unix.Mkdirat(fd, c, 0o755); err == nil {
					err = a.chown(fd, c)
				}
			}
			if err == nil {
				nfd, err = unix.Openat(fd, c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
		}
		_ = unix.Close(fd)
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return -1, errors.New("a folder in its path is a link or a file on the host")
		}
		if err != nil {
			return -1, err
		}
		fd = nfd
		if err := a.sameFS(fd); err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
	}
	return fd, nil
}

func (a *applier) sameFS(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if uint64(st.Dev) != a.dev { //nolint:unconvert // Dev is int32 on darwin
		return errors.New("its folder is on another filesystem")
	}
	return nil
}

// mine refuses a folder the owner does not own.
func (a *applier) mine(fd int) error {
	if a.owner == nil {
		return nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != a.owner.UID {
		return errors.New("its folder is not owned by the workspace's user")
	}
	return nil
}

func (a *applier) chown(dirfd int, name string) error {
	if a.owner == nil {
		return nil
	}
	return unix.Fchownat(dirfd, name, int(a.owner.UID), int(a.owner.GID), unix.AT_SYMLINK_NOFOLLOW)
}

func lstatAt(dirfd int, name string) (*unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, nil
		}
		return nil, err
	}
	return &st, nil
}

func isDir(st *unix.Stat_t) bool { return st != nil && st.Mode&unix.S_IFMT == unix.S_IFDIR }

// clear removes whatever is at name so something else can take its place;
// a folder holding a protected path cannot be cleared.
func (a *applier) clear(pfd int, comps []string) error {
	name := comps[len(comps)-1]
	st, err := lstatAt(pfd, name)
	if err != nil || st == nil {
		return err
	}
	if !a.removeAt(pfd, comps, st) {
		return errors.New("what is there on the host could not all be removed")
	}
	return nil
}

func (a *applier) mkdir(comps []string, mode int64) error {
	pfd, err := a.parent(comps, true)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(pfd) }()
	if err := a.mine(pfd); err != nil {
		return err
	}
	name := comps[len(comps)-1]
	st, err := lstatAt(pfd, name)
	if err != nil {
		return err
	}
	if !isDir(st) {
		if err := a.clear(pfd, comps); err != nil {
			return err
		}
		if err := unix.Mkdirat(pfd, name, 0o700); err != nil {
			return err
		}
		if err := a.chown(pfd, name); err != nil {
			return err
		}
	}
	fd, err := unix.Openat(pfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := a.sameFS(fd); err != nil {
		return err
	}
	if err := a.mine(fd); err != nil {
		return errors.New("the host's folder is not owned by the workspace's user")
	}
	return unix.Fchmod(fd, uint32(mode)&0o777)
}

func tempName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return ".abhed-sync-" + hex.EncodeToString(b)
}

// replace puts the new entry made at tmp in place of name.
func (a *applier) replace(pfd int, comps []string, tmp string) error {
	name := comps[len(comps)-1]
	st, err := lstatAt(pfd, name)
	if err == nil && st != nil {
		if a.owner != nil && st.Uid != a.owner.UID {
			err = errors.New("the host's copy is not owned by the workspace's user")
		} else if isDir(st) {
			err = a.clear(pfd, comps)
		}
	}
	if err == nil {
		err = unix.Renameat(pfd, tmp, pfd, name)
	}
	if err != nil {
		_ = unix.Unlinkat(pfd, tmp, 0)
	}
	return err
}

func (a *applier) file(comps []string, h *tar.Header, body io.Reader) error {
	if isGitFile(comps) {
		return a.skip(strings.Join(comps, "/"), "a .git file points git elsewhere and is write-protected")
	}
	if h.Size < 0 || a.written+h.Size > a.max {
		a.written = a.max + 1
		return fmt.Errorf("the guest sent more than the %d bytes a sync may carry", a.max)
	}
	pfd, err := a.parent(comps, true)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(pfd) }()
	if err := a.mine(pfd); err != nil {
		return err
	}
	tmp := tempName()
	fd, err := unix.Openat(pfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmp)
	n, err := io.Copy(f, io.LimitReader(body, h.Size))
	a.written += n
	if err == nil && n != h.Size {
		err = errors.New("the guest sent less than it said")
	}
	if err == nil && a.owner != nil {
		err = f.Chown(int(a.owner.UID), int(a.owner.GID))
	}
	if err == nil {
		// Permission bits only: no setuid, setgid or sticky bit comes back.
		err = f.Chmod(os.FileMode(h.Mode) & 0o777)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = unix.Unlinkat(pfd, tmp, 0)
		return err
	}
	return a.replace(pfd, comps, tmp)
}

func (a *applier) symlink(comps []string, target string) error {
	rel := strings.Join(comps, "/")
	if isGitFile(comps) {
		return a.skip(rel, "a .git link points git elsewhere and is write-protected")
	}
	// A link may only name something inside the workspace, and nothing
	// protected, so no later write through it on the host escapes.
	if target == "" || path.IsAbs(target) || strings.ContainsRune(target, 0) {
		return a.skip(rel, "a link to "+target+" points outside the workspace")
	}
	resolved := path.Join(path.Dir(rel), target)
	tcomps, err := cleanRel(resolved)
	if err != nil {
		return a.skip(rel, "a link to "+target+" points outside the workspace")
	}
	if why := a.guard.refuses(tcomps); why != "" {
		return a.skip(rel, "a link to "+target+" points at a protected path")
	}
	pfd, err := a.parent(comps, true)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(pfd) }()
	if err := a.mine(pfd); err != nil {
		return err
	}
	tmp := tempName()
	if err := unix.Symlinkat(target, pfd, tmp); err != nil {
		return err
	}
	if err := a.chown(pfd, tmp); err != nil {
		_ = unix.Unlinkat(pfd, tmp, 0)
		return err
	}
	return a.replace(pfd, comps, tmp)
}

func (a *applier) remove(comps []string) error {
	pfd, err := a.parent(comps, false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(pfd) }()
	if err := a.mine(pfd); err != nil {
		return err
	}
	st, err := lstatAt(pfd, comps[len(comps)-1])
	if err != nil || st == nil {
		return err
	}
	a.removeAt(pfd, comps, st)
	return nil
}

// removeAt removes comps' last part from pfd, a folder's contents first,
// keeping and recording anything protected. It reports whether all went.
func (a *applier) removeAt(pfd int, comps []string, st *unix.Stat_t) bool {
	rel := strings.Join(comps, "/")
	name := comps[len(comps)-1]
	if a.owner != nil && st.Uid != a.owner.UID {
		a.refuse(rel, "not owned by the workspace's user; kept")
		return false
	}
	if !isDir(st) {
		if err := unix.Unlinkat(pfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			a.refuse(rel, err.Error())
			return false
		}
		return true
	}
	fd, err := unix.Openat(pfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		a.refuse(rel, err.Error())
		return false
	}
	dir := os.NewFile(uintptr(fd), rel)
	defer func() { _ = dir.Close() }()
	if err := a.sameFS(fd); err != nil {
		a.refuse(rel, err.Error()+"; kept")
		return false
	}
	names, err := dir.Readdirnames(-1)
	if err != nil {
		a.refuse(rel, err.Error())
		return false
	}
	all := true
	for _, n := range names {
		child := append(append([]string{}, comps...), n)
		if why := a.guard.refuses(child); why != "" {
			a.refuse(strings.Join(child, "/"), why+"; kept")
			all = false
			continue
		}
		cst, err := lstatAt(fd, n)
		if err != nil || cst == nil {
			continue
		}
		if !a.removeAt(fd, child, cst) {
			all = false
		}
	}
	if !all {
		return false
	}
	if err := unix.Unlinkat(pfd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		a.refuse(rel, err.Error())
		return false
	}
	return true
}
