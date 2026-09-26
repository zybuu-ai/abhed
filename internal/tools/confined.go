package tools

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutside refuses a path, or a link it leads through, that leaves the root.
var ErrOutside = errors.New("the path leads outside the workspace")

// outside names an os.Root refusal of a link that left the root as
// ErrOutside; os does not export its own.
func outside(err error) error {
	if err != nil && strings.Contains(err.Error(), "path escapes from parent") {
		return ErrOutside
	}
	return err
}

// Confined opens files under one workspace root, held open as an os.Root: a
// link, even one swapped in after the path was checked, is followed only while
// it stays under the root, and a folder or file that is Abhed's state is
// refused by identity once opened.
type Confined struct {
	root  *os.Root
	dirs  []string // the root's spellings: resolved, then as given
	state *StateSet
	// The folder ReadEntry last read from, kept open for the next entry.
	walkDir *os.Root
	walkRel string
}

// Confine opens dir as a root. alt are other spellings of the same folder
// (before links resolve), so a path written either way can be placed in it.
func (set *StateSet) Confine(dir string, alt ...string) (*Confined, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	dirs := []string{filepath.Clean(dir)}
	for _, a := range alt {
		if a != "" {
			dirs = append(dirs, filepath.Clean(a))
		}
	}
	return &Confined{root: r, dirs: dirs, state: set}, nil
}

func (c *Confined) Close() {
	if c.walkDir != nil {
		_ = c.walkDir.Close()
	}
	_ = c.root.Close()
}

// rel places an absolute path under the root, or refuses it.
func (c *Confined) rel(path string) (string, error) {
	for _, d := range c.dirs {
		if rel, err := filepath.Rel(d, filepath.Clean(path)); err == nil && filepath.IsLocal(rel) {
			return rel, nil
		}
	}
	return "", ErrOutside
}

// Contains reports whether an absolute path is spelled under the root.
func (c *Confined) Contains(path string) bool {
	_, err := c.rel(path)
	return err == nil
}

// folder opens the folder holding rel, confined to the root, and refuses a
// state folder.
func (c *Confined) folder(rel string) (*os.Root, error) {
	d, err := c.root.OpenRoot(filepath.Dir(rel))
	if err != nil {
		return nil, outside(err)
	}
	if info, err := d.Stat("."); err != nil || c.state.HasFile(info) {
		_ = d.Close()
		if err != nil {
			return nil, err
		}
		return nil, ErrState
	}
	return d, nil
}

// OpenRead opens a file under the root for reading. A link in the last part
// is followed one hop at a time, each hop kept under the root and judged, and
// the file opened must be the one looked at and not a state file.
func (c *Confined) OpenRead(path string) (*os.File, error) {
	rel, err := c.rel(path)
	if err != nil {
		return nil, err
	}
	for hop := 0; hop < 40; hop++ {
		if c.state.Has(filepath.Join(c.dirs[0], rel)) {
			return nil, ErrState
		}
		d, err := c.folder(rel)
		if err != nil {
			return nil, err
		}
		base := filepath.Base(rel)
		looked, err := d.Lstat(base)
		if err == nil && looked.Mode()&fs.ModeSymlink != 0 {
			target, lerr := d.Readlink(base)
			_ = d.Close()
			if lerr != nil {
				return nil, lerr
			}
			// A relative target is taken against the folder's path under the
			// root, not against a folder a link above it leads to, so it can
			// name another file than the system would; what opens is still
			// under the root and judged.
			if filepath.IsAbs(target) {
				rel, err = c.rel(target)
			} else if rel = filepath.Join(filepath.Dir(rel), target); !filepath.IsLocal(rel) {
				err = ErrOutside
			}
			if err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			_ = d.Close()
			return nil, err
		}
		f, err := d.Open(base)
		_ = d.Close()
		if err != nil {
			return nil, outside(err)
		}
		info, err := f.Stat()
		if err != nil || c.state.HasFile(info) || !os.SameFile(info, looked) {
			_ = f.Close()
			if err != nil {
				return nil, err
			}
			return nil, ErrState
		}
		return f, nil
	}
	return nil, errors.New("too many links")
}

// ReadFile reads a file through OpenRead.
func (c *Confined) ReadFile(path string) ([]byte, error) {
	f, err := c.OpenRead(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// WriteAtomic writes a file through a temporary file renamed into place, in
// its folder opened under the root and judged by identity. The rename
// replaces the name, and never writes into another file it linked to.
func (c *Confined) WriteAtomic(path string, data []byte, mode os.FileMode) error {
	rel, err := c.rel(path)
	if err != nil {
		return err
	}
	if c.state.Has(filepath.Join(c.dirs[0], rel)) {
		return ErrState
	}
	d, err := c.folder(rel)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	tmp, name, err := createTemp(d)
	if err != nil {
		return err
	}
	defer func() { _ = d.Remove(name) }() // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return d.Rename(name, filepath.Base(rel))
}

// CreateNew creates a file that must not exist yet, in its folder opened
// under the root and judged by identity.
func (c *Confined) CreateNew(path string, data []byte, mode os.FileMode) error {
	rel, err := c.rel(path)
	if err != nil {
		return err
	}
	if c.state.Has(filepath.Join(c.dirs[0], rel)) {
		return ErrState
	}
	d, err := c.folder(rel)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	f, err := d.OpenFile(filepath.Base(rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Remove removes a file under the root, by its name in its folder opened
// under the root: a link there is removed, not what it points at.
func (c *Confined) Remove(path string) error {
	rel, err := c.rel(path)
	if err != nil {
		return err
	}
	if c.state.Has(filepath.Join(c.dirs[0], rel)) {
		return ErrState
	}
	d, err := c.folder(rel)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Remove(filepath.Base(rel))
}

// MkdirAll creates a folder and its parents under the root one step at a time,
// each step judged by name and, once opened, by identity before anything is made in it.
func (c *Confined) MkdirAll(path string, perm os.FileMode) error {
	rel, err := c.rel(path)
	if err != nil || rel == "." {
		return err
	}
	parent, err := c.root.OpenRoot(".")
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		sofar := filepath.Join(parts[:i+1]...)
		if c.state.Has(filepath.Join(c.dirs[0], sofar)) {
			return ErrState
		}
		if err := parent.Mkdir(part, perm); err != nil && !errors.Is(err, fs.ErrExist) {
			return outside(err)
		}
		next, err := c.root.OpenRoot(sofar)
		if err != nil {
			return outside(err)
		}
		_ = parent.Close()
		parent = next
		if info, err := parent.Stat("."); err != nil || c.state.HasFile(info) {
			if err != nil {
				return err
			}
			return ErrState
		}
	}
	return nil
}

// ReadInWorkspace reads a file of the workspace as the file tools read it:
// under the workspace held open, refusing a link that leads out of it or into
// Abhed's state.
func ReadInWorkspace(workspace, path string) ([]byte, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	c, err := NewStateSet(abs).Confine(RealPath(abs), abs)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.ReadFile(path)
}

// ReadEntry reads a file met while walking the root, by its path relative to
// it, if it is still the entry walked and not a state file. Cheaper than
// ReadFile: the walk has already judged the path, so only identity is asked.
func (c *Confined) ReadEntry(rel string, walked os.FileInfo) ([]byte, error) {
	// A walk reads a folder's files one after another, so the folder is kept
	// open between them.
	if dir := filepath.Dir(rel); c.walkDir == nil || c.walkRel != dir {
		if c.walkDir != nil {
			_ = c.walkDir.Close()
			c.walkDir = nil
		}
		d, err := c.folder(rel)
		if err != nil {
			return nil, err
		}
		c.walkDir, c.walkRel = d, dir
	}
	f, err := c.walkDir.Open(filepath.Base(rel))
	if err != nil {
		return nil, outside(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, walked) || c.state.HasFile(info) {
		return nil, ErrState
	}
	return io.ReadAll(f)
}

func createTemp(d *os.Root) (*os.File, string, error) {
	for i := 0; ; i++ {
		var b [8]byte
		_, _ = rand.Read(b[:])
		name := ".abhed-" + hex.EncodeToString(b[:])
		f, err := d.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil || !errors.Is(err, fs.ErrExist) || i > 8 {
			return f, name, err
		}
	}
}
