//go:build !windows

package guest

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// slack is how far back a change is looked for before the last sync began:
// an inode's change time comes from a clock up to one kernel tick behind.
const slack = 50 * time.Millisecond

// Tracker remembers what the host was last sent of the folder at root.
type Tracker struct {
	root string

	mu    sync.Mutex
	since time.Time
	known map[string]bool
}

// NewTracker starts from root as it is now: the host already has it. It
// waits out the clock's lag first, so what setup changed is not sent back.
func NewTracker(root string) (*Tracker, error) {
	time.Sleep(slack)
	t := &Tracker{root: root, since: time.Now()}
	names, err := t.names()
	if err != nil {
		return nil, err
	}
	t.known = names
	return t, nil
}

// skip is what is never synced: ext4's lost+found at the top.
func skip(rel string) bool { return rel == "lost+found" }

func (t *Tracker) names() (map[string]bool, error) {
	out := map[string]bool{}
	err := filepath.WalkDir(t.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(t.root, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if skip(rel) {
			return filepath.SkipDir
		}
		out[rel] = true
		return nil
	})
	return out, err
}

// export writes the changes since the last sync to c and, once the host
// acknowledges them, records them as synced.
func (t *Tracker) export(c *conn) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	start := time.Now()
	now, err := t.names()
	if err != nil {
		return err
	}
	var gone []string
	for p := range t.known {
		if !now[p] {
			gone = append(gone, p)
		}
	}
	// Deepest first, so a folder's contents go before it.
	sort.Sort(sort.Reverse(sort.StringSlice(gone)))
	for _, p := range gone {
		if err := c.write(frame{Type: tDelete, Path: p}); err != nil {
			return err
		}
	}
	var names []string
	for p := range now {
		names = append(names, p)
	}
	// Parents first.
	sort.Strings(names)
	tw := tar.NewWriter(frameWriter{c, tTar})
	for _, p := range names {
		if err := t.add(tw, p, !t.known[p]); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := c.write(frame{Type: tEOF}); err != nil {
		return err
	}
	f, err := c.read()
	if err != nil {
		return err
	}
	if f.Type != tAck {
		return fmt.Errorf("%w: expected ack, got %q", errProtocol, f.Type)
	}
	t.known, t.since = now, start.Add(-slack)
	return nil
}

// add writes p to tw when it is new or changed since the last sync.
func (t *Tracker) add(tw *tar.Writer, p string, isNew bool) error {
	full := filepath.Join(t.root, filepath.FromSlash(p))
	fi, err := os.Lstat(full)
	if err != nil {
		// Gone since the walk: the next sync deletes it.
		return nil
	}
	if !isNew && changed(fi).Before(t.since) {
		return nil
	}
	h := &tar.Header{Name: p, Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime(), Format: tar.FormatPAX}
	switch {
	case fi.Mode().IsRegular():
		h.Typeflag, h.Size = tar.TypeReg, fi.Size()
	case fi.IsDir():
		h.Typeflag, h.Name = tar.TypeDir, p+"/"
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return nil
		}
		h.Typeflag, h.Linkname = tar.TypeSymlink, target
	default:
		// Devices, pipes and sockets stay in the guest.
		return nil
	}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	if h.Typeflag != tar.TypeReg {
		return nil
	}
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// A file that shrank while being sent fails the sync, which is tried
	// again next time; one that grew is sent as it was when stat'd.
	if _, err := io.CopyN(tw, f, h.Size); err != nil {
		return fmt.Errorf("sending %s: %w", p, err)
	}
	return nil
}
