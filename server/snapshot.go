package server

import (
	"bytes"
	"crypto/sha256"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// The changes view shows what the edit tools changed from their undo log. A
// shell command, the person's or the agent's, changes files without it, so
// each session also keeps a snapshot of the workspace as it opened: every
// file's size, time and hash, and the text of small files within a budget.
// A file that differs from it, and that the undo log does not hold, is a
// change made some other way.

const (
	// snapMaxFiles bounds the walk; past it the snapshot says it is partial.
	snapMaxFiles = 20000
	// snapKeepFile and snapBudget bound the text kept to diff against.
	snapKeepFile = 1 << 20
	snapBudget   = 16 << 20
)

type snapFile struct {
	size int64
	mod  time.Time
	sum  [sha256.Size]byte
	// hashed is set once sum is known; a file changed before it was read
	// as the session opened never is, and is compared by its stat alone.
	hashed bool
	// text is the content as the session opened, when it was kept.
	text []byte
	kept bool
}

// seenFile is a file as a Changes poll last found it, so the next poll
// reads only what changed again.
type seenFile struct {
	size int64
	mod  time.Time
	sum  [sha256.Size]byte
	// data is the content, kept when it is text small enough to diff.
	data     []byte
	diffable bool
}

type wsSnapshot struct {
	mu    sync.Mutex
	root  string
	files map[string]*snapFile // by slash path, relative to root
	seen  map[string]*seenFile
	// partial says why files past it are not compared, when they are not.
	partial string
	// filled closes once every file's content has been read.
	filled chan struct{}
}

// takeSnapshot records the workspace at root as it is now. Only each file's
// size and time are taken before it returns, so a session is not held up by
// a large workspace; the contents are read after, and a file found changed
// by then is compared by its stat alone.
func takeSnapshot(root string) *wsSnapshot {
	s := &wsSnapshot{root: root, files: map[string]*snapFile{}, seen: map[string]*seenFile{}, filled: make(chan struct{})}
	var order []string
	s.partial = walkWorkspace(root, func(rel string, info fs.FileInfo, _ func() ([]byte, error)) {
		s.files[rel] = &snapFile{size: info.Size(), mod: info.ModTime()}
		order = append(order, rel)
	})
	go s.fill(order)
	return s
}

// fill reads each file's content as the session opened, while it is unchanged.
func (s *wsSnapshot) fill(order []string) {
	defer close(s.filled)
	r, err := os.OpenRoot(s.root)
	if err != nil {
		return
	}
	defer func() { _ = r.Close() }()
	budget := snapBudget
	for _, rel := range order {
		b, err := fs.ReadFile(r.FS(), rel)
		if err != nil {
			continue
		}
		info, err := r.Stat(rel)
		if err != nil {
			continue
		}
		s.mu.Lock()
		f := s.files[rel]
		if f != nil && !f.hashed && f.size == info.Size() && f.mod.Equal(info.ModTime()) && int64(len(b)) == f.size {
			f.sum, f.hashed = sha256.Sum256(b), true
			if len(b) <= snapKeepFile && len(b) <= budget && !tools.IsBinary(b) {
				f.text, f.kept = b, true
				budget -= len(b)
			}
		}
		s.mu.Unlock()
	}
}

// walkWorkspace visits every regular file under root that the viewer could
// show: not in .git, node_modules or .abhed, not Abhed's state, and not
// through a link. It returns why it stopped early, or "".
func walkWorkspace(root string, visit func(rel string, info fs.FileInfo, read func() ([]byte, error))) string {
	r, err := os.OpenRoot(root)
	if err != nil {
		return "the workspace could not be read"
	}
	defer func() { _ = r.Close() }()
	var state *tools.StateSet
	if sess, err := tools.NewSession(root); err == nil {
		state = sess.FreezeState()
	}
	n, partial := 0, ""
	_ = fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is left out, not the end of the walk
		}
		if p != "." && skipInView(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		abs := filepath.Join(root, filepath.FromSlash(p))
		if state != nil && state.Has(abs) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if n++; n > snapMaxFiles {
			partial = "the workspace has more than 20,000 files"
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // a file gone since it was listed is left out
		}
		visit(p, info, func() ([]byte, error) { return fs.ReadFile(r.FS(), p) })
		return nil
	})
	return partial
}

// shellChange is a file that differs from the snapshot.
type shellChange struct {
	rel    string
	status string // added | modified | deleted
	before []byte
	after  []byte
	// diffable is set when both sides are known as text.
	diffable bool
}

// changes compares the workspace now with the snapshot, leaving out paths in
// skip. A file is read only when its stat differs from both the snapshot and
// the last poll.
func (s *wsSnapshot) changes(skip map[string]bool) ([]shellChange, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []shellChange
	found := map[string]bool{}
	partial := walkWorkspace(s.root, func(rel string, info fs.FileInfo, read func() ([]byte, error)) {
		found[rel] = true
		was, ok := s.files[rel]
		if skip[rel] || ok && was.size == info.Size() && was.mod.Equal(info.ModTime()) {
			delete(s.seen, rel)
			return
		}
		now := s.seen[rel]
		if now == nil || now.size != info.Size() || !now.mod.Equal(info.ModTime()) {
			b, err := read()
			if err != nil {
				return
			}
			now = &seenFile{size: info.Size(), mod: info.ModTime(), sum: sha256.Sum256(b)}
			if now.diffable = !tools.IsBinary(b) && len(b) <= maxViewBytes; now.diffable {
				now.data = b
			}
			s.seen[rel] = now
		}
		switch {
		case !ok:
			out = append(out, shellChange{rel: rel, status: "added", after: now.data, diffable: now.diffable})
		case !was.hashed:
			out = append(out, shellChange{rel: rel, status: "modified"})
		case now.sum != was.sum:
			out = append(out, shellChange{rel: rel, status: "modified", before: was.text, after: now.data, diffable: was.kept && now.diffable})
		}
	})
	for rel := range s.seen {
		if !found[rel] {
			delete(s.seen, rel)
		}
	}
	if partial == "" {
		partial = s.partial
	}
	for rel, was := range s.files {
		if !found[rel] && !skip[rel] && partial == "" {
			out = append(out, shellChange{rel: rel, status: "deleted", before: was.text, diffable: was.kept})
		}
	}
	return out, partial
}

// original is what the session opened with at rel, when rel has changed
// since and the snapshot kept it: the baseline of a change a command made.
func (s *wsSnapshot) original(rel string) (text []byte, existed, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now, err := readIn(s.root, rel)
	f, ok := s.files[rel]
	switch {
	case !ok && (err != nil || s.partial != ""):
		return nil, false, false // never there, or past what the snapshot covers
	case !ok:
		return nil, false, true // a file a command created
	case !f.hashed:
		return nil, true, false
	case err == nil && sha256.Sum256(now) == f.sum:
		return nil, false, false // unchanged
	}
	return f.text, true, f.kept
}

// readIn reads rel under root, without following a link out of it.
func readIn(root, rel string) ([]byte, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return fs.ReadFile(r.FS(), rel)
}

// accept moves rel's baseline to content, as the undo log's Accept does. The
// file on disk may differ from it, so it is always compared by hash.
func (s *wsSnapshot) accept(rel string, content []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[rel] = &snapFile{size: -1, sum: sha256.Sum256(content), hashed: true, text: bytes.Clone(content), kept: true}
	delete(s.seen, rel)
}

// relSlash is a workspace path as the snapshot keys it.
func relSlash(rel string) string {
	return path.Clean(strings.ReplaceAll(rel, string(filepath.Separator), "/"))
}
