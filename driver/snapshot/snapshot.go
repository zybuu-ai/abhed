// Package snapshot keeps workspace snapshots for drivers: each is a
// gzip-compressed tar of the workspace, without Abhed's state, in a folder
// of the operator's outside every workspace, named by its SHA-256. A restore
// checks that digest, unpacks into a fresh folder inside the workspace with
// the checks a copy gets, checks the digest again over what it read, and
// only then replaces the workspace's contents, keeping .abhed as it is.
package snapshot

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/archive"
)

// stagingPrefix starts the folder a restore unpacks into, inside the
// workspace so the swap is a rename; it is never snapshotted.
const stagingPrefix = ".abhed-restore-"

const idPrefix = "snap_"

// Store holds snapshots under Dir, one folder per sandbox.
type Store struct {
	// Dir is an absolute folder outside every workspace, reachable only by
	// the server's user.
	Dir    string
	Limits archive.Limits
	// Owner, when set, owns what a restore writes; for a server running as
	// root whose sandboxes run as the workspace's owner.
	Owner *archive.Owner
}

// ValidID reports whether id has the form a snapshot id takes.
func ValidID(id string) bool {
	if len(id) != len(idPrefix)+16 || !strings.HasPrefix(id, idPrefix) {
		return false
	}
	for _, c := range id[len(idPrefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ValidName reports whether a snapshot's optional name is acceptable:
// up to 63 characters from [A-Za-z0-9._-].
func ValidName(name string) bool {
	if len(name) > 63 {
		return false
	}
	for _, c := range name {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return idPrefix + hex.EncodeToString(b[:])
}

// sandboxDir is the sandbox's folder in the store; ids are checked by the
// caller's driver, and checked again here since they name a folder.
func (s Store) sandboxDir(sandboxID string) (string, error) {
	if s.Dir == "" || !filepath.IsAbs(s.Dir) {
		return "", driver.Errorf(driver.ErrUnavailable, "no snapshot folder is configured")
	}
	if sandboxID == "" || sandboxID != filepath.Base(sandboxID) || strings.HasPrefix(sandboxID, ".") {
		return "", driver.Errorf(driver.ErrInvalid, "sandbox id %q cannot name a folder", sandboxID)
	}
	return filepath.Join(s.Dir, sandboxID), nil
}

// check refuses a store inside the workspace or holding it.
func (s Store) check(workspace string) error {
	dir, err := filepath.EvalSymlinks(s.Dir)
	if err != nil {
		dir = filepath.Clean(s.Dir)
	}
	ws, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return driver.Errorf(driver.ErrUnavailable, "the workspace: %v", err)
	}
	if within(dir, ws) || within(ws, dir) {
		return driver.Errorf(driver.ErrUnavailable, "the snapshot folder %s overlaps the workspace %s", s.Dir, workspace)
	}
	return nil
}

func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Take snapshots workspace for the sandbox at generation gen.
func (s Store) Take(ctx context.Context, sandboxID string, gen int64, name, workspace string) (driver.SnapshotRef, archive.Stats, error) {
	if !ValidName(name) {
		return driver.SnapshotRef{}, archive.Stats{}, driver.Errorf(driver.ErrInvalid, "snapshot name %q: up to 63 of A-Z a-z 0-9 . _ -", name)
	}
	dir, err := s.sandboxDir(sandboxID)
	if err != nil {
		return driver.SnapshotRef{}, archive.Stats{}, err
	}
	if err := s.check(workspace); err != nil {
		return driver.SnapshotRef{}, archive.Stats{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return driver.SnapshotRef{}, archive.Stats{}, driver.Errorf(driver.ErrUnavailable, "the snapshot folder: %v", err)
	}
	tmp, err := os.CreateTemp(dir, ".taking-*")
	if err != nil {
		return driver.SnapshotRef{}, archive.Stats{}, driver.Errorf(driver.ErrUnavailable, "the snapshot folder: %v", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	sum := sha256.New()
	counted := &counter{w: io.MultiWriter(tmp, sum)}
	zw := gzip.NewWriter(counted)
	st, err := archive.Pack(ctx, workspace, zw, s.Limits, func(name string) bool {
		return !strings.Contains(name, "/") && strings.HasPrefix(name, stagingPrefix)
	})
	if err != nil {
		return driver.SnapshotRef{}, st, err
	}
	if err := zw.Close(); err != nil {
		return driver.SnapshotRef{}, st, driver.Errorf(driver.ErrUnavailable, "writing the snapshot: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		return driver.SnapshotRef{}, st, driver.Errorf(driver.ErrUnavailable, "writing the snapshot: %v", err)
	}
	ref := driver.SnapshotRef{ID: newID(), Name: name, Generation: gen, Digest: digest(sum),
		Bytes: counted.n, CreatedAt: time.Now().UTC()}
	if err := os.Rename(tmp.Name(), s.file(dir, ref.ID)); err != nil {
		return driver.SnapshotRef{}, st, driver.Errorf(driver.ErrUnavailable, "writing the snapshot: %v", err)
	}
	return ref, st, nil
}

func (s Store) file(dir, id string) string { return filepath.Join(dir, id+".tar.gz") }

// Remove deletes one snapshot of the sandbox.
func (s Store) Remove(sandboxID string, ref driver.SnapshotRef) error {
	dir, err := s.sandboxDir(sandboxID)
	if err != nil {
		return err
	}
	if !ValidID(ref.ID) {
		return driver.Errorf(driver.ErrInvalid, "snapshot id %q is not valid", ref.ID)
	}
	if err := os.Remove(s.file(dir, ref.ID)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return driver.Errorf(driver.ErrNotFound, "no snapshot %s of %s", ref.ID, sandboxID)
		}
		return driver.Errorf(driver.ErrUnavailable, "removing the snapshot: %v", err)
	}
	return nil
}

// RemoveAll deletes every snapshot of the sandbox, as its deletion does.
func (s Store) RemoveAll(sandboxID string) error {
	dir, err := s.sandboxDir(sandboxID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return driver.Errorf(driver.ErrUnavailable, "removing the snapshots: %v", err)
	}
	return nil
}

// Restore replaces workspace's contents with the snapshot ref of the
// sandbox. A ref of another sandbox is not found; content whose digest
// differs from ref.Digest is refused before the workspace is touched.
func (s Store) Restore(ctx context.Context, sandboxID string, ref driver.SnapshotRef, workspace string) (archive.Stats, error) {
	dir, err := s.sandboxDir(sandboxID)
	if err != nil {
		return archive.Stats{}, err
	}
	if !ValidID(ref.ID) {
		return archive.Stats{}, driver.Errorf(driver.ErrInvalid, "snapshot id %q is not valid", ref.ID)
	}
	want, err := parseDigest(ref.Digest)
	if err != nil {
		return archive.Stats{}, err
	}
	if err := s.check(workspace); err != nil {
		return archive.Stats{}, err
	}
	f, err := os.Open(s.file(dir, ref.ID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return archive.Stats{}, driver.Errorf(driver.ErrNotFound, "no snapshot %s of %s", ref.ID, sandboxID)
		}
		return archive.Stats{}, driver.Errorf(driver.ErrUnavailable, "opening the snapshot: %v", err)
	}
	defer f.Close()
	// A first pass refuses a mismatch before anything is written.
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return archive.Stats{}, driver.Errorf(driver.ErrUnavailable, "reading the snapshot: %v", err)
	}
	if digest(sum) != want {
		return archive.Stats{}, mismatch(ref)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return archive.Stats{}, driver.Errorf(driver.ErrUnavailable, "reading the snapshot: %v", err)
	}

	ws, err := os.OpenRoot(workspace)
	if err != nil {
		return archive.Stats{}, driver.Errorf(driver.ErrUnavailable, "the workspace: %v", err)
	}
	defer ws.Close()
	staging := stagingPrefix + hex.EncodeToString(randBytes(6))
	if err := ws.Mkdir(staging, 0o700); err != nil {
		return archive.Stats{}, driver.Errorf(driver.ErrUnavailable, "the workspace: %v", err)
	}
	swapped := false
	defer func() {
		if !swapped {
			_ = ws.RemoveAll(staging)
		}
	}()
	stage, err := ws.OpenRoot(staging)
	if err != nil {
		return archive.Stats{}, driver.Errorf(driver.ErrUnavailable, "the workspace: %v", err)
	}
	defer stage.Close()

	// The second pass hashes what it unpacks; the swap happens only if
	// that, too, is the snapshot the ref names.
	sum.Reset()
	zr, err := gzip.NewReader(io.TeeReader(f, sum))
	if err != nil {
		return archive.Stats{}, driver.Errorf(driver.ErrInvalid, "the snapshot is not a gzip stream: %v", err)
	}
	st, err := archive.Unpack(ctx, zr, stage, s.Limits, s.Owner)
	if err != nil {
		return st, err
	}
	if _, err := io.Copy(io.Discard, f); err != nil {
		return st, driver.Errorf(driver.ErrUnavailable, "reading the snapshot: %v", err)
	}
	if digest(sum) != want {
		return st, mismatch(ref)
	}
	if err := ctx.Err(); err != nil {
		return st, err
	}
	swapped = true
	if err := swap(ws, staging); err != nil {
		return st, err
	}
	return st, nil
}

func mismatch(ref driver.SnapshotRef) error {
	return driver.Errorf(driver.ErrInvalid, "snapshot %s does not match its digest %s; it is not restored", ref.ID, ref.Digest)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func digest(h hash.Hash) string { return "sha256:" + hex.EncodeToString(h.Sum(nil)) }

func parseDigest(d string) (string, error) {
	hexPart, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(hexPart) != 64 {
		return "", driver.Errorf(driver.ErrInvalid, "snapshot digest %q is not sha256:<64 hex>; a snapshot without one is not restored", d)
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return "", driver.Errorf(driver.ErrInvalid, "snapshot digest %q is not hex", d)
	}
	return "sha256:" + strings.ToLower(hexPart), nil
}

// swap clears the workspace except Abhed's state and the staging folder,
// then moves the staged contents in. Folders that hold a .abhed further
// down are kept and merged into, so no state folder is ever touched. A
// clash with one of them is found before anything is removed.
func swap(ws *os.Root, staging string) error {
	kept, err := keptDirs(ws, ".", staging)
	if err != nil {
		return err
	}
	if err := clashes(ws, staging, ".", kept); err != nil {
		return err
	}
	if err := clearAll(ws, ".", staging, kept); err != nil {
		return err
	}
	if err := move(ws, staging, "."); err != nil {
		return err
	}
	if err := ws.RemoveAll(staging); err != nil {
		return driver.Errorf(driver.ErrUnavailable, "removing the restore's folder: %v", err)
	}
	return nil
}

func join(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}

func readDir(ws *os.Root, dir string) ([]fs.DirEntry, error) {
	d, err := ws.Open(dir)
	if err != nil {
		return nil, driver.Errorf(driver.ErrUnavailable, "reading %s: %v", dir, err)
	}
	defer d.Close()
	es, err := d.ReadDir(-1)
	if err != nil {
		return nil, driver.Errorf(driver.ErrUnavailable, "reading %s: %v", dir, err)
	}
	return es, nil
}

// keptDirs are the folders, as slash paths, that hold a .abhed somewhere
// below them, and so stay.
func keptDirs(ws *os.Root, dir, staging string) (map[string]bool, error) {
	kept := map[string]bool{}
	var walk func(dir string) (bool, error)
	walk = func(dir string) (bool, error) {
		es, err := readDir(ws, dir)
		if err != nil {
			return false, err
		}
		holds := false
		for _, e := range es {
			p := join(dir, e.Name())
			switch {
			case strings.EqualFold(e.Name(), archive.StateDir):
				holds = true
			case dir == "." && e.Name() == staging:
			case e.IsDir(): // DirEntry types are not followed through links
				h, err := walk(p)
				if err != nil {
					return false, err
				}
				if h {
					kept[p] = true
					holds = true
				}
			}
		}
		return holds, nil
	}
	_, err := walk(dir)
	return kept, err
}

// clashes refuses a staged entry that is not a folder where a kept folder is.
func clashes(ws *os.Root, staging, dir string, kept map[string]bool) error {
	es, err := readDir(ws, join(staging, dir))
	if err != nil {
		return err
	}
	for _, e := range es {
		p := join(dir, e.Name())
		if !kept[p] {
			continue
		}
		if !e.IsDir() {
			return driver.Errorf(driver.ErrConflict, "the snapshot has a file at %s, where a folder holding Abhed's state is; nothing was changed", p)
		}
		if err := clashes(ws, staging, p, kept); err != nil {
			return err
		}
	}
	return nil
}

func clearAll(ws *os.Root, dir, staging string, kept map[string]bool) error {
	es, err := readDir(ws, dir)
	if err != nil {
		return err
	}
	for _, e := range es {
		p := join(dir, e.Name())
		switch {
		case strings.EqualFold(e.Name(), archive.StateDir):
		case dir == "." && e.Name() == staging:
		case kept[p]:
			if err := clearAll(ws, p, staging, kept); err != nil {
				return err
			}
		default:
			if err := ws.RemoveAll(p); err != nil {
				return driver.Errorf(driver.ErrUnavailable, "clearing %s: %v", p, err)
			}
		}
	}
	return nil
}

func move(ws *os.Root, staging, dir string) error {
	es, err := readDir(ws, join(staging, dir))
	if err != nil {
		return err
	}
	for _, e := range es {
		p := join(dir, e.Name())
		if info, err := ws.Lstat(p); err == nil && info.IsDir() && e.IsDir() {
			if err := move(ws, staging, p); err != nil {
				return err
			}
			continue
		}
		if err := ws.Rename(path.Join(staging, p), p); err != nil {
			return driver.Errorf(driver.ErrUnavailable, "restoring %s: %v", p, err)
		}
	}
	return nil
}

type counter struct {
	w io.Writer
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Payload is the record's view of a snapshot.
func Payload(sandboxID string, gen int64, ref driver.SnapshotRef, st archive.Stats) map[string]any {
	p := map[string]any{"sandbox_id": sandboxID, "generation": gen, "snapshot_id": ref.ID,
		"snapshot_generation": ref.Generation, "digest": ref.Digest, "bytes": ref.Bytes,
		"entries": st.Entries, "skipped": st.Skipped}
	if ref.Name != "" {
		p["name"] = ref.Name
	}
	if ref.Image != "" {
		p["image"] = ref.Image
	}
	return p
}
