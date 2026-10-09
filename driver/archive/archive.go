// Package archive checks and writes the tar streams drivers move in and out
// of a workspace: copies and snapshots. Every archive is untrusted, so each
// entry is checked before anything is written for it, and unpacking goes
// through an os.Root, which no link or ".." can leave.
package archive

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

// StateDir is Abhed's state folder in a workspace. No archive reads or
// writes it, under any case.
const StateDir = ".abhed"

// Limits bound an archive. Zero fields take the defaults.
type Limits struct {
	Entries int
	Bytes   int64
}

// Default limits: large enough for a workspace with its dependencies.
const (
	DefaultEntries = 1_000_000
	DefaultBytes   = 16 << 30
)

func (l Limits) entries() int {
	if l.Entries > 0 {
		return l.Entries
	}
	return DefaultEntries
}

func (l Limits) bytes() int64 {
	if l.Bytes > 0 {
		return l.Bytes
	}
	return DefaultBytes
}

// Stats counts what an archive held.
type Stats struct {
	Entries int   `json:"entries"`
	Bytes   int64 `json:"bytes"`
	// Skipped are entries left out of a pack: links leading out of the
	// folder, sockets, devices and pipes.
	Skipped int `json:"skipped,omitempty"`
}

// Name is an entry's name cleaned, or an error when it is absolute or
// climbs out of the folder it is unpacked into.
func Name(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) || strings.Contains(name, `\`) {
		return "", driver.Errorf(driver.ErrInvalid, "archive entry %q is not a relative path", name)
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", driver.Errorf(driver.ErrInvalid, "archive entry %q climbs out of the folder", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", driver.Errorf(driver.ErrInvalid, "archive entry %q holds ..", name)
		}
	}
	return clean, nil
}

// Link refuses a symbolic link whose target is absolute or resolves above
// the folder the archive is unpacked into.
func Link(name, target string) error {
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsRune(target, 0) {
		return driver.Errorf(driver.ErrInvalid, "link %s points to %q, outside the folder", name, target)
	}
	joined := path.Clean(path.Join(path.Dir(name), target))
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return driver.Errorf(driver.ErrInvalid, "link %s points to %q, outside the folder", name, target)
	}
	return nil
}

// InState reports whether a slash-separated name is, or is inside, a
// folder named .abhed in any case.
func InState(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, StateDir) {
			return true
		}
	}
	return false
}

// TypeName names a tar entry type for an error.
func TypeName(t byte) string {
	switch t {
	case tar.TypeChar:
		return "character device"
	case tar.TypeBlock:
		return "block device"
	case tar.TypeFifo:
		return "named pipe"
	}
	return "entry of type " + string(rune(t))
}

// Pack writes the folder dir as a tar stream: folders, regular files and
// links that stay inside it. Abhed's state and names skip reports true are
// left out, and so are links leading out, sockets, devices and pipes, which
// Stats.Skipped counts. A file that changes size while it is read fails the
// pack, since the archive would not hold what it says.
func Pack(ctx context.Context, dir string, w io.Writer, lim Limits, skip func(name string) bool) (Stats, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Stats{}, driver.Errorf(driver.ErrUnavailable, "opening %s: %v", dir, err)
	}
	defer root.Close()
	fsys := root.FS()
	tw := tar.NewWriter(w)
	var st Stats
	err = fs.WalkDir(fsys, ".", func(name string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && name != "." {
				return nil // removed while the folder was read
			}
			return driver.Errorf(driver.ErrUnavailable, "reading %s: %v", name, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if InState(name) || (skip != nil && skip(name)) {
			if e.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return driver.Errorf(driver.ErrUnavailable, "reading %s: %v", name, err)
		}
		h := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), ModTime: info.ModTime().UTC(), Format: tar.FormatPAX}
		switch {
		case info.Mode().IsDir():
			h.Typeflag, h.Name = tar.TypeDir, name+"/"
		case info.Mode().IsRegular():
			h.Typeflag = tar.TypeReg
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := root.Readlink(name)
			if err != nil {
				return driver.Errorf(driver.ErrUnavailable, "reading link %s: %v", name, err)
			}
			if Link(name, target) != nil {
				st.Skipped++
				return nil
			}
			h.Typeflag, h.Linkname = tar.TypeSymlink, target
		default:
			st.Skipped++
			return nil
		}
		st.Entries++
		if st.Entries > lim.entries() {
			return driver.Errorf(driver.ErrInvalid, "the folder has more than %d entries", lim.entries())
		}
		if h.Typeflag != tar.TypeReg {
			return writeHeader(tw, h)
		}
		return packFile(root, tw, h, &st, lim)
	})
	if err != nil {
		return st, err
	}
	if err := tw.Close(); err != nil {
		return st, driver.Errorf(driver.ErrUnavailable, "writing the archive: %v", err)
	}
	return st, nil
}

func writeHeader(tw *tar.Writer, h *tar.Header) error {
	if err := tw.WriteHeader(h); err != nil {
		return driver.Errorf(driver.ErrUnavailable, "writing the archive: %v", err)
	}
	return nil
}

// packFile writes one regular file, sized from the open file itself.
func packFile(root *os.Root, tw *tar.Writer, h *tar.Header, st *Stats, lim Limits) error {
	f, err := root.Open(h.Name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			st.Entries--
			return nil
		}
		return driver.Errorf(driver.ErrUnavailable, "reading %s: %v", h.Name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return driver.Errorf(driver.ErrConflict, "%s changed while the folder was read", h.Name)
	}
	h.Size = info.Size()
	st.Bytes += h.Size
	if st.Bytes > lim.bytes() {
		return driver.Errorf(driver.ErrInvalid, "the folder holds more than %d bytes", lim.bytes())
	}
	if err := writeHeader(tw, h); err != nil {
		return err
	}
	if n, err := io.CopyN(tw, f, h.Size); err != nil {
		if n < h.Size && (err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF)) {
			return driver.Errorf(driver.ErrConflict, "%s changed while it was read; stop the sandbox for a consistent snapshot", h.Name)
		}
		return driver.Errorf(driver.ErrUnavailable, "reading %s: %v", h.Name, err)
	}
	return nil
}

type pending struct {
	name, target string
	mode         fs.FileMode
	mtime        time.Time
}

// Owner is who unpacked entries belong to; nil leaves them the caller's.
type Owner struct{ UID, GID int }

// Unpack writes a tar stream into root, which should be empty. Entries get
// the checks a copy gets (relative names, no "..", links that stay inside,
// no devices or pipes) and none may be in Abhed's state. Files are created,
// never replaced; hard and symbolic links are made after every file, so no
// write goes through a link the archive made. Setuid, setgid and sticky bits
// are cleared, and owners are the caller's unless own is set.
func Unpack(ctx context.Context, src io.Reader, root *os.Root, lim Limits, own *Owner) (Stats, error) {
	tr := tar.NewReader(src)
	var (
		st                     Stats
		dirs, files            []pending
		hardLinks, symbolLinks []pending
	)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return st, driver.Errorf(driver.ErrInvalid, "reading the archive: %v", err)
		}
		if err := ctx.Err(); err != nil {
			return st, err
		}
		st.Entries++
		if st.Entries > lim.entries() {
			return st, driver.Errorf(driver.ErrInvalid, "the archive has more than %d entries", lim.entries())
		}
		name, err := Name(h.Name)
		if err != nil {
			return st, err
		}
		if name == "." {
			continue
		}
		if InState(name) {
			return st, driver.Errorf(driver.ErrInvalid, "archive entry %s is in Abhed's state, which is never written", name)
		}
		p := pending{name: name, mode: fs.FileMode(h.Mode).Perm(), mtime: h.ModTime}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := mkdirAll(root, name); err != nil {
				return st, err
			}
			dirs = append(dirs, p)
		case tar.TypeReg, 0: // 0 is the old flag for a regular file
			st.Bytes += h.Size
			if h.Size < 0 || st.Bytes > lim.bytes() {
				return st, driver.Errorf(driver.ErrInvalid, "the archive holds more than %d bytes", lim.bytes())
			}
			if err := writeFile(root, name, tr, h.Size); err != nil {
				return st, err
			}
			files = append(files, p)
		case tar.TypeLink:
			target, err := Name(h.Linkname)
			if err != nil {
				return st, driver.Errorf(driver.ErrInvalid, "hard link %s: %v", name, err)
			}
			p.target = target
			hardLinks = append(hardLinks, p)
		case tar.TypeSymlink:
			if err := Link(name, h.Linkname); err != nil {
				return st, err
			}
			p.target = h.Linkname
			symbolLinks = append(symbolLinks, p)
		default:
			return st, driver.Errorf(driver.ErrInvalid, "archive entry %s is a %s, which is not restored", name, TypeName(h.Typeflag))
		}
	}
	for _, l := range hardLinks {
		if InState(l.target) {
			return st, driver.Errorf(driver.ErrInvalid, "hard link %s points into Abhed's state", l.name)
		}
		info, err := root.Lstat(l.target)
		if err != nil || !info.Mode().IsRegular() {
			return st, driver.Errorf(driver.ErrInvalid, "hard link %s points to %s, which is not a file in the archive", l.name, l.target)
		}
		if err := mkdirAll(root, path.Dir(l.name)); err != nil {
			return st, err
		}
		if err := root.Link(l.target, l.name); err != nil {
			return st, driver.Errorf(driver.ErrInvalid, "hard link %s: %v", l.name, err)
		}
	}
	for _, l := range symbolLinks {
		if err := mkdirAll(root, path.Dir(l.name)); err != nil {
			return st, err
		}
		if err := root.Symlink(l.target, l.name); err != nil {
			return st, driver.Errorf(driver.ErrInvalid, "link %s: %v", l.name, err)
		}
	}
	if own != nil {
		for _, group := range [][]pending{dirs, files, hardLinks, symbolLinks} {
			for _, p := range group {
				if err := root.Lchown(p.name, own.UID, own.GID); err != nil {
					return st, driver.Errorf(driver.ErrUnavailable, "owning %s: %v", p.name, err)
				}
			}
		}
	}
	for _, f := range files {
		_ = root.Chmod(f.name, f.mode)
		_ = root.Chtimes(f.name, f.mtime, f.mtime)
	}
	// Deepest first, so a read-only folder is closed after what is in it.
	slices.SortFunc(dirs, func(a, b pending) int { return strings.Count(b.name, "/") - strings.Count(a.name, "/") })
	for _, d := range dirs {
		_ = root.Chmod(d.name, d.mode|0o700)
		_ = root.Chtimes(d.name, d.mtime, d.mtime)
	}
	return st, nil
}

func mkdirAll(root *os.Root, name string) error {
	if name == "." || name == "" {
		return nil
	}
	if err := root.MkdirAll(name, 0o700); err != nil {
		return driver.Errorf(driver.ErrInvalid, "folder %s: %v", name, err)
	}
	return nil
}

func writeFile(root *os.Root, name string, r io.Reader, size int64) error {
	if err := mkdirAll(root, path.Dir(name)); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return driver.Errorf(driver.ErrInvalid, "file %s: %v", name, err)
	}
	_, cerr := io.CopyN(f, r, size)
	if err := f.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return driver.Errorf(driver.ErrInvalid, "file %s: %v", name, cerr)
	}
	return nil
}
