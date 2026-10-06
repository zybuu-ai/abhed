package local

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Index operations. The index is append-only: a later line about a session
// changes what an earlier one said, and nothing is rewritten.
const (
	opCreate = "create" // a new session, with where and for whom
	opTitle  = "title"  // its first prompt, redacted and cut
	opName   = "name"   // a name given by -n or /rename
	opEnd    = "end"    // a run ended, with the head at that point
	opOpen   = "open"   // a process claimed it to go on
	opPrune  = "prune"  // the tombstone: the file was removed, its head kept
	opRepair = "repair" // an unfinished last index line was cut off
	opBranch = "branch" // the session began as a copy of another
	opActive = "active" // the conversation went on: a later prompt, or a run's replies
)

// kindSubagent marks a subagent's own session.
const kindSubagent = "subagent"

// indexLine is one line of index.jsonl, chained like a session's lines.
type indexLine struct {
	N         int64  `json:"n"`
	Op        string `json:"op"`
	ID        string `json:"id"`
	At        string `json:"at"`
	Cwd       string `json:"cwd,omitempty"`
	Repo      string `json:"repo,omitempty"`
	GitBranch string `json:"git_branch,omitempty"`
	Parent    string `json:"parent,omitempty"`
	ForkSeq   int64  `json:"fork_seq,omitempty"`
	User      string `json:"user,omitempty"`
	Model     string `json:"model,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Title     string `json:"title,omitempty"`
	Name      string `json:"name,omitempty"`
	Ended     string `json:"ended,omitempty"`
	By        string `json:"by,omitempty"`
	Reason    string `json:"reason,omitempty"`
	HeadLines int64  `json:"head_lines,omitempty"`
	HeadSeq   int64  `json:"head_seq,omitempty"`
	HeadHash  string `json:"head_hash,omitempty"`
	// A tombstone says whether the file was already gone, and whether the
	// record verified when it was pruned.
	Missing    bool   `json:"missing,omitempty"`
	Verified   *bool  `json:"verified,omitempty"`
	Unverified string `json:"unverified,omitempty"`
	Prev       string `json:"prev"`
	Hash       string `json:"hash,omitempty"`
}

func (l *indexLine) seal() ([]byte, error) {
	l.Hash = ""
	body, err := encode(l)
	if err != nil {
		return nil, err
	}
	l.Hash = hashBytes(body)
	return encode(l)
}

// afterIndexSentinel runs once a new index's sentinel head is written; a
// test looks at the head there.
var afterIndexSentinel = func() {}

// ErrIndexDamaged is an index that no longer matches its head or its own
// chain. Nothing is added to it until it is looked at: abhed record verify
// names the line.
var ErrIndexDamaged = errors.New("the record's index fails verification")

// index is a tenant's index.jsonl, its head and the lock every process
// takes to append to it.
type index struct {
	// ok is how much of the file has been checked, and the hash of each
	// line checked, so an append checks only what was added since.
	mu     sync.Mutex
	ok     int64
	hashes []string
	dir    string
	// clock is the time written on each line; nil is the wall clock.
	clock func() time.Time
	// onHead is given each new head, for an anchor.
	onHead func(Head)

	// cache is the parsed index, and folded what it says per session.
	cmu        sync.Mutex
	cache      indexCache
	folded     []Entry
	foldedFrom int
}

// indexCache is the index as last read: its size and time, how far its
// whole lines go, and those lines parsed.
type indexCache struct {
	size  int64
	mtime time.Time
	off   int64
	lines []indexLine
}

func (x *index) path() string     { return filepath.Join(x.dir, "index.jsonl") }
func (x *index) headPath() string { return filepath.Join(x.dir, "index.head") }
func (x *index) lockPath() string { return filepath.Join(x.dir, "index.lock") }

func (x *index) now() time.Time {
	if x.clock != nil {
		return x.clock()
	}
	return time.Now().UTC()
}

// append adds one line under the index lock: numbered and chained after the
// last line, synced, and the head moved to it.
func (x *index) append(l indexLine) error {
	lk, err := openOwn(x.lockPath(), os.O_CREATE|os.O_RDWR)
	if err != nil {
		return fmt.Errorf("lock the index: %w", err)
	}
	defer func() { _ = lk.Close() }()
	if err := waitLock(lk); err != nil {
		return fmt.Errorf("lock the index: %w", err)
	}
	defer func() { _ = unlock(lk) }()

	f, err := openOwn(x.path(), os.O_CREATE|os.O_RDWR|os.O_APPEND)
	if err != nil {
		return fmt.Errorf("open the index: %w", err)
	}
	defer func() { _ = f.Close() }()
	// The index is checked against its head before anything is added: an
	// append onto a cut or broken index would make the damage look sound.
	n, prev, torn, err := x.check(f)
	if err != nil {
		return err
	}
	// A new index gets its sentinel head before its first line.
	if _, have := readHeadFile(x.headPath()); n == 0 && !have {
		if err := writeHeadFile(x.headPath(), sentinelHead); err != nil {
			return err
		}
		afterIndexSentinel()
	}
	if torn > 0 {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if err := f.Truncate(info.Size() - torn); err != nil {
			return fmt.Errorf("repair the index: %w", err)
		}
		rep := indexLine{N: n + 1, Op: opRepair, ID: "-", At: x.now().Format(timeFormat), Reason: fmt.Sprintf("an unfinished last line of %d bytes was cut off", torn), Prev: prev}
		raw, err := rep.seal()
		if err != nil {
			return err
		}
		if _, err := f.Write(append(raw, '\n')); err != nil {
			return err
		}
		prev, n = rep.Hash, rep.N
		x.remember(rep.Hash, int64(len(raw)+1))
	}
	l.N, l.Prev = n+1, prev
	if l.At == "" {
		l.At = x.now().Format(timeFormat)
	}
	raw, err := l.seal()
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		x.forget()
		return fmt.Errorf("write the index: %w", err)
	}
	x.remember(l.Hash, int64(len(raw)+1))
	if err := syncFile(f); err != nil {
		return err
	}
	head := Head{Lines: l.N, Hash: l.Hash}
	if err := writeHeadFile(x.headPath(), head); err != nil {
		return err
	}
	if x.onHead != nil {
		x.onHead(head)
	}
	return nil
}

// check verifies what was added to the index since the last check, and the
// head against it, and returns the line count, the last hash and the size of
// an unfinished last line. The caller holds the index lock.
func (x *index) check(f *os.File) (int64, string, int64, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	info, err := f.Stat()
	if err != nil {
		return 0, "", 0, err
	}
	size := info.Size()
	if size < x.ok {
		x.ok, x.hashes = 0, nil // shrank: check it all again
	}
	data := make([]byte, size-x.ok)
	if _, err := f.ReadAt(data, x.ok); err != nil && !errors.Is(err, io.EOF) {
		return 0, "", 0, err
	}
	sc := scan(data)
	damaged := func(line int64, why string) (int64, string, int64, error) {
		x.ok, x.hashes = 0, nil
		return 0, "", 0, fmt.Errorf("%w: line %d: %s; see abhed record verify", ErrIndexDamaged, line, why)
	}
	prev := Genesis
	if n := len(x.hashes); n > 0 {
		prev = x.hashes[n-1]
	}
	checked := x.ok
	for _, raw := range sc.raws {
		n := int64(len(x.hashes)) + 1
		l, err := parseIndexLine(raw)
		if err != nil {
			return damaged(n, "the line is not an index entry")
		}
		again := l
		sealed, err := again.seal()
		switch {
		case err != nil || again.Hash != l.Hash || !bytes.Equal(sealed, raw):
			return damaged(n, "its hash does not match its content")
		case l.N != n || l.Prev != prev:
			return damaged(n, "it does not follow the line before it")
		}
		prev = l.Hash
		x.hashes = append(x.hashes, l.Hash)
		checked += int64(len(raw)) + 1
	}
	x.ok = checked
	n := int64(len(x.hashes))
	head, have := readHeadFile(x.headPath())
	switch {
	case !have && n > 0:
		return damaged(n, "the index head is missing or malformed")
	case have && head.Lines == 0 && n > 1:
		return damaged(2, sentinelBehind)
	case have && head.Lines > n:
		return damaged(n+1, fmt.Sprintf("lines are missing: its head says it held %d, it holds %d", head.Lines, n))
	case have && head.Lines > 0 && x.hashes[head.Lines-1] != head.Hash:
		return damaged(head.Lines, "the line is not the one its head recorded")
	}
	return n, prev, int64(len(sc.tail)), nil
}

// remember adds a line this process wrote to what has been checked.
func (x *index) remember(hash string, size int64) {
	x.mu.Lock()
	x.hashes = append(x.hashes, hash)
	x.ok += size
	x.mu.Unlock()
}

// forget drops what was checked, after a write that may have been partial.
func (x *index) forget() {
	x.mu.Lock()
	x.ok, x.hashes = 0, nil
	x.mu.Unlock()
}

func parseIndexLine(raw []byte) (indexLine, error) {
	var l indexLine
	dec := jsonStrict(raw)
	if err := dec.Decode(&l); err != nil {
		return l, err
	}
	return l, nil
}

// lines reads every complete index line; a damaged one is skipped here and
// named by verify. What was read is kept, keyed by the file's size and time,
// and only what was added since is read again.
func (x *index) lines() ([]indexLine, error) {
	f, err := openOwn(x.path(), os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	x.cmu.Lock()
	defer x.cmu.Unlock()
	c := &x.cache
	if info.Size() == c.size && info.ModTime().Equal(c.mtime) {
		return c.lines, nil
	}
	if info.Size() < c.off {
		*c = indexCache{} // shrank: read it all again
	}
	data := make([]byte, info.Size()-c.off)
	if _, err := f.ReadAt(data, c.off); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	sc := scan(data)
	lines := slices.Clone(c.lines)
	off := c.off
	for _, raw := range sc.raws {
		if l, err := parseIndexLine(raw); err == nil {
			lines = append(lines, l)
		}
		off += int64(len(raw)) + 1
	}
	*c = indexCache{size: info.Size(), mtime: info.ModTime(), off: off, lines: lines}
	return lines, nil
}

// entries folds the index into one entry per session, in first-seen order.
// The fold is kept with the lines it came from.
func (x *index) entries() ([]Entry, error) {
	ls, err := x.lines()
	if err != nil {
		return nil, err
	}
	x.cmu.Lock()
	if x.folded != nil && x.foldedFrom == len(ls) {
		out := slices.Clone(x.folded)
		x.cmu.Unlock()
		return out, nil
	}
	x.cmu.Unlock()
	out := fold(ls)
	x.cmu.Lock()
	x.folded, x.foldedFrom = slices.Clone(out), len(ls)
	x.cmu.Unlock()
	return out, nil
}

// fold is what the index says about each session, in first-seen order.
func fold(ls []indexLine) []Entry {
	byID := map[string]*Entry{}
	activeSeen := false
	var order []string
	for _, l := range ls {
		at, _ := time.Parse(timeFormat, l.At)
		e := byID[l.ID]
		if e == nil {
			if l.Op != opCreate {
				continue
			}
			e = &Entry{ID: l.ID, Created: at}
			byID[l.ID] = e
			order = append(order, l.ID)
		}
		e.Updated = at
		switch l.Op {
		case opCreate, opTitle, opActive:
			e.Active = at
		case opEnd:
			// Ends written before this index held any active line, by a version
			// that wrote none: a run's end is the best those say.
			if !activeSeen {
				e.Active = at
			}
		}
		if l.Op == opActive {
			activeSeen = true
		}
		switch l.Op {
		case opCreate:
			e.Cwd, e.Repo, e.GitBranch, e.User = l.Cwd, l.Repo, l.GitBranch, l.User
			e.Parent, e.ForkSeq, e.Subagent = l.Parent, l.ForkSeq, l.Kind == kindSubagent
			if l.Title != "" {
				e.Title = l.Title
			}
		case opTitle:
			if e.Title == "" {
				e.Title = l.Title
			}
		case opName:
			e.Name = l.Name
		case opEnd:
			e.Ended = l.Ended
			e.Head = Head{Lines: l.HeadLines, Seq: l.HeadSeq, Hash: l.HeadHash}
		case opOpen:
			e.Ended = ""
		case opBranch:
			e.Parent, e.ForkSeq = l.Parent, l.ForkSeq
		case opPrune:
			e.Pruned = true
			e.Head = Head{Lines: l.HeadLines, Seq: l.HeadSeq, Hash: l.HeadHash}
		}
	}
	out := make([]Entry, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

func (x *index) get(id string) (Entry, bool) {
	all, err := x.entries()
	if err != nil {
		return Entry{}, false
	}
	for _, e := range all {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// list selects sessions, most recently active first: never a subagent's own
// session, and never a pruned one.
func (x *index) list(f Filter) ([]Entry, error) {
	all, err := x.entries()
	if err != nil {
		return nil, err
	}
	cwd, repo := cleanDir(f.Cwd), ""
	if f.Repo && !f.All {
		repo = RepoOf(f.Cwd)
	}
	var out []Entry
	for _, e := range all {
		if e.Subagent || e.Pruned {
			continue
		}
		switch {
		case f.All:
		case repo != "":
			if e.Repo != repo && cleanDir(e.Cwd) != cwd {
				continue
			}
		case cleanDir(e.Cwd) != cwd:
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Active.After(out[j].Active) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func cleanDir(d string) string {
	if d == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(d); err == nil {
		d = r
	}
	return filepath.Clean(d)
}

// indexView is the Store's SessionIndex.
type indexView struct{ s *Store }

func (v indexView) List(f Filter) ([]Entry, error) {
	out, err := v.s.index.list(f)
	for i := range out {
		if h, ok := v.s.readHead(out[i].ID); ok {
			out[i].Head = h
		}
	}
	return out, err
}

func (v indexView) Get(id string) (Entry, error) {
	e, ok := v.s.index.get(id)
	if !ok {
		return Entry{}, fmt.Errorf("session %s: %w", id, ErrNotFound)
	}
	if h, ok := v.s.readHead(id); ok {
		e.Head = h
	}
	return e, nil
}

// Resolve finds a session by id, name, unique id prefix, or the path of its
// file in this records directory.
func (v indexView) Resolve(key string) (Entry, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return Entry{}, ErrNotFound
	}
	if strings.ContainsRune(key, filepath.Separator) || strings.HasSuffix(key, ".jsonl") {
		abs, err := filepath.Abs(key)
		if err != nil {
			return Entry{}, err
		}
		if cleanDir(filepath.Dir(abs)) != cleanDir(v.s.dir) {
			return Entry{}, fmt.Errorf("%s is not in this records directory: %w", key, ErrNotFound)
		}
		key = strings.TrimSuffix(filepath.Base(abs), ".jsonl")
	}
	all, err := v.s.index.entries()
	if err != nil {
		return Entry{}, err
	}
	var live []Entry
	for _, e := range all {
		if !e.Pruned {
			live = append(live, e)
		}
	}
	for _, e := range live {
		if e.ID == key {
			return v.Get(e.ID)
		}
	}
	for _, match := range []func(Entry) bool{
		func(e Entry) bool { return e.Name == key },
		func(e Entry) bool { return len(key) >= 4 && strings.HasPrefix(e.ID, key) },
	} {
		var hit []Entry
		for _, e := range live {
			if match(e) {
				hit = append(hit, e)
			}
		}
		switch len(hit) {
		case 0:
			continue
		case 1:
			return v.Get(hit[0].ID)
		default:
			return Entry{}, fmt.Errorf("%q: %w", key, ErrAmbiguous)
		}
	}
	return Entry{}, fmt.Errorf("session %q: %w", key, ErrNotFound)
}

func (v indexView) Head(id string) (int64, string, error) {
	h, ok := v.s.readHead(id)
	if !ok {
		return 0, "", fmt.Errorf("session %s: %w", id, ErrNotFound)
	}
	return h.Seq, h.Hash, nil
}
