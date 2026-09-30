package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// Options open a Store.
type Options struct {
	// Dir is the records directory; "" is ~/.abhed/records. Only the managed
	// configuration may move it (record.dir).
	Dir string
	// Tenant names the subdirectory sessions are kept in; "" is "default".
	Tenant string
	// User is who sessions created here are recorded for.
	User string
	// Redact rewrites every payload before its first write, as the
	// Recorder's redactor does, so a writer that forgot one still cannot put
	// a stored secret on disk. Nil leaves payloads as the Recorder made them.
	Redact agent.Redactor
	// Clock is the time written on index lines; nil is the wall clock.
	Clock func() time.Time
	// Anchor, when set, is given each head as it is synced: a session's, or
	// the index's under the id "index". It is where a witness outside this
	// machine can take the chain, which makes the record evident against the
	// machine's owner too; the record does not depend on it, and an error it
	// has is its own to report.
	Anchor func(tenant, session string, head Head)
}

// DefaultDir is ~/.abhed/records.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory for the record: %w", err)
	}
	return filepath.Join(home, ".abhed", "records"), nil
}

// Store is the local record: one chained file per session under
// <dir>/<tenant>, an index, heads, locks and blobs. It is safe for
// concurrent use; several processes share a directory, one writer per
// session.
type Store struct {
	root, dir    string
	tenant, user string
	redact       agent.Redactor
	anchor       func(tenant, session string, head Head)

	mu     sync.Mutex
	held   map[string]*session
	subs   map[string][]chan agent.Event
	closed bool

	blobs *blobStore
	index *index
}

// session is one session this process holds the lock for, with its events.
type session struct {
	mu     sync.Mutex
	id     string
	f      *os.File
	lock   *os.File
	events []agent.Event
	seqs   map[int64]string // seq -> event id
	last   Head
	synced Head
	// running is set from a claim or creation until an end with nothing left
	// in the background, as a Postgres session row's ended_at is cleared.
	running bool
	titled  bool
	child   bool
	repair  *agent.RecordRepaired
}

var (
	_ Record                  = (*Store)(nil)
	_ agent.SessionCreator    = (*Store)(nil)
	_ agent.SubSessionChecker = (*Store)(nil)
)

// Open opens, creating when needed, the records directory and the tenant's
// part of it. Directories are 0700 and files 0600; a directory another user
// owns is refused.
func Open(opts Options) (*Store, error) {
	root := opts.Dir
	if root == "" {
		d, err := DefaultDir()
		if err != nil {
			return nil, err
		}
		root = d
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	tenant := opts.Tenant
	if tenant == "" {
		tenant = "default"
	}
	if err := checkID("tenant", tenant); err != nil {
		return nil, err
	}
	if err := privateDir(root); err != nil {
		return nil, err
	}
	// A records directory that is a link is used, and protected, by where it
	// really is; the folders inside it may not be links.
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	dir := filepath.Join(root, tenant)
	for _, d := range []string{dir, filepath.Join(dir, "head"), filepath.Join(dir, "locks"), filepath.Join(dir, "blobs")} {
		if info, err := os.Lstat(d); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a link; the record's folders must be its own", d)
		}
		if err := privateDir(d); err != nil {
			return nil, err
		}
	}
	// The marker tells the state walk this folder is the record, which it
	// guards by the folder rather than file by file.
	marker := filepath.Join(root, tools.RecordMarker)
	if _, err := os.Lstat(marker); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(marker, []byte("Abhed's local record. Check it with: abhed record verify\n"), 0o600); err != nil {
			return nil, err
		}
	}
	anchor := opts.Anchor
	red := opts.Redact
	if v := reflect.ValueOf(red); red != nil && v.Kind() == reflect.Pointer && v.IsNil() {
		red = nil // a typed nil redacts nothing, and must not be called
	}
	s := &Store{
		root: root, dir: dir, tenant: tenant, user: opts.User, redact: red, anchor: anchor,
		held: map[string]*session{}, subs: map[string][]chan agent.Event{},
	}
	s.blobs = &blobStore{dir: filepath.Join(dir, "blobs", "sha256")}
	s.index = &index{dir: dir, clock: opts.Clock}
	if anchor != nil {
		s.index.onHead = func(h Head) { anchor(tenant, "index", h) }
	}
	return s, nil
}

// privateDir makes dir, owner-only, and refuses one another user owns.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if err := ownedByMe(info); err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a directory, owner-only
			return fmt.Errorf("make %s private: %w", dir, err)
		}
	}
	return nil
}

// Dir is the records directory, and Tenant the tenant in it.
func (s *Store) Dir() string    { return s.root }
func (s *Store) Tenant() string { return s.tenant }

// Path is where session id's file is.
func (s *Store) Path(id string) string { return filepath.Join(s.dir, id+".jsonl") }

func (s *Store) headPath(id string) string { return filepath.Join(s.dir, "head", id) }
func (s *Store) lockPath(id string) string { return filepath.Join(s.dir, "locks", id+".lock") }

// Index lists and finds sessions.
func (s *Store) Index() SessionIndex { return indexView{s} }

// Blobs holds checkpoint pre-images.
func (s *Store) Blobs() Blobs { return s.blobs }

// ErrClosed is a Store used after Close.
var ErrClosed = errors.New("the record is closed")

// Acquire takes the one writer's lock on session id for this process, the
// file created if it does not exist. ErrHeldElsewhere means another Abhed
// process is writing it; the caller may offer a fork instead. An unfinished
// last line a crash left is cut off and a record.repaired event appended.
func (s *Store) Acquire(id string) error {
	_, err := s.acquire(id, false, nil)
	return err
}

func (s *Store) acquire(id string, create bool, entry *indexLine) (*session, error) {
	if err := checkID("session", id); err != nil {
		return nil, err
	}
	h, err := s.take(id, create, entry)
	if err != nil || h.repair == nil {
		return h, err
	}
	// Recorded once the session is held, at the next seq.
	rep := *h.repair
	h.repair = nil
	if err := s.appendHeld(h, s.systemEvent(h, agent.EvRecordRepaired, rep)); err != nil {
		return nil, err
	}
	return h, nil
}

// take locks session id for this process and loads it.
func (s *Store) take(id string, create bool, entry *indexLine) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if h := s.held[id]; h != nil {
		if create {
			return nil, store.ErrSessionExists
		}
		return h, nil
	}
	lk, err := os.OpenFile(s.lockPath(id), os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- an id checked to be a plain name
	if err != nil {
		return nil, fmt.Errorf("lock session %s: %w", id, err)
	}
	got, err := tryLock(lk)
	if err != nil || !got {
		_ = lk.Close()
		if err != nil {
			return nil, fmt.Errorf("lock session %s: %w", id, err)
		}
		return nil, fmt.Errorf("session %s: %w", id, ErrHeldElsewhere)
	}
	known, found := s.index.get(id)
	if found && known.Pruned {
		_ = unlock(lk)
		_ = lk.Close()
		return nil, fmt.Errorf("session %s was pruned; its tombstone is all that remains", id)
	}
	h, err := s.load(id, lk, create)
	if err != nil {
		_ = unlock(lk)
		_ = lk.Close()
		return nil, err
	}
	switch {
	case create || entry != nil || !found:
		// A session first written without CreateSession is listed all the same.
		if entry == nil {
			entry = &indexLine{}
		}
		entry.Op, entry.ID = opCreate, id
		if entry.User == "" {
			entry.User = s.user
		}
		if err := s.index.append(*entry); err != nil {
			_ = h.f.Close()
			_ = unlock(lk)
			_ = lk.Close()
			return nil, err
		}
		h.child = entry.Kind == kindSubagent
	default:
		h.titled, h.child = known.Title != "", known.Subagent
	}
	s.held[id] = h
	return h, nil
}

// load opens a session's file for appending and reads what it holds.
func (s *Store) load(id string, lk *os.File, create bool) (*session, error) {
	path := s.Path(id)
	flags := os.O_RDWR | os.O_APPEND | os.O_CREATE
	if create {
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o600) // #nosec G304 -- an id checked to be a plain name
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, store.ErrSessionExists
		}
		return nil, fmt.Errorf("open session %s: %w", id, err)
	}
	h := &session{id: id, f: f, lock: lk, seqs: map[int64]string{}, last: Head{Hash: Genesis}}
	fail := func(err error) (*session, error) {
		_ = f.Close()
		return nil, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- as above
	if err != nil {
		return fail(err)
	}
	sc := scan(data)
	head, have := s.readHead(id)
	counted := int64(len(sc.raws))
	// Decide the repair, check the record as it would then stand, and write
	// only if it verifies: a refused record is left exactly as found.
	const (
		keep = iota
		complete
		cut
	)
	action := keep
	if len(sc.tail) > 0 {
		switch {
		case completesChain(sc):
			// A whole line that lost only its newline is completed, not cut.
			action = complete
			sc.raws = append(sc.raws, sc.tail)
		case have && head.Lines > counted:
			// The head counts a line the file no longer holds whole: that is
			// a cut, not a crash, and it is left as evidence.
		default:
			// A crash cut a line the head never counted: it can go.
			action = cut
		}
	}
	rep, lines := verifyLines(sc.raws, id)
	if len(sc.tail) > 0 && action == keep && rep.OK {
		rep.OK, rep.FirstBad = false, rep.Head.Seq+1
		rep.Reason = "the last line the head counts is cut short"
	}
	if len(sc.raws) > 0 || have {
		checkHead(&rep, lines, head, have)
	}
	if !rep.OK {
		return fail(&UnverifiedError{Report: rep})
	}
	switch action {
	case complete:
		if _, err := f.Write([]byte{'\n'}); err != nil {
			return fail(fmt.Errorf("repair session %s: %w", id, err))
		}
		h.repair = &agent.RecordRepaired{Reason: "a last line without its newline was completed"}
	case cut:
		if err := f.Truncate(int64(len(data) - len(sc.tail))); err != nil {
			return fail(fmt.Errorf("repair session %s: %w", id, err))
		}
		h.repair = &agent.RecordRepaired{Reason: "an unfinished last line, left by a crash, was cut off", TruncatedBytes: int64(len(sc.tail))}
	}
	if h.repair != nil {
		if err := syncFile(f); err != nil {
			return fail(err)
		}
	}
	for _, l := range lines {
		ev := l.event()
		h.events = append(h.events, ev)
		h.seqs[ev.Seq] = ev.ID
	}
	sortEvents(h.events)
	h.last = rep.Head
	if h.last.Lines == 0 {
		h.last.Hash = Genesis
	}
	// The head only ever moves forward, to lines the chain holds.
	h.synced = head
	if !have || h.last.Lines > head.Lines {
		if err := s.writeHead(id, h.last); err != nil {
			return fail(err)
		}
		h.synced = h.last
	}
	return h, nil
}

// completesChain reports whether a file's unfinished tail is a whole sealed
// line that follows the last complete one, and lost only its newline.
func completesChain(sc scanned) bool {
	l, why := checkLine(sc.tail)
	if why != "" {
		return false
	}
	prev := Genesis
	if n := len(sc.raws); n > 0 {
		last, err := parseLine(sc.raws[n-1])
		if err != nil {
			return false
		}
		prev = last.Hash
	}
	return l.Prev == prev
}

// ErrUnverified is a session whose record fails verification. It is read,
// never written to; going on from it is a fork into a new session.
var ErrUnverified = errors.New("the record fails verification")

// UnverifiedError says why a session's record cannot be written to.
type UnverifiedError struct{ Report Report }

func (e *UnverifiedError) Error() string {
	return fmt.Sprintf("session %s: %v: at seq %d, %s", e.Report.ID, ErrUnverified, e.Report.FirstBad, e.Report.Reason)
}

func (e *UnverifiedError) Unwrap() error { return ErrUnverified }

// Release gives up this process's lock on a session, so another process
// may continue it. The session's file stays as it is.
func (s *Store) Release(id string) error {
	s.mu.Lock()
	h := s.held[id]
	delete(s.held, id)
	s.mu.Unlock()
	if h == nil {
		return nil
	}
	return h.close(s)
}

// Held reports whether this process holds session id's lock.
func (s *Store) Held(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held[id] != nil
}

func (h *session) close(s *Store) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	err := h.sync(s)
	if cerr := h.f.Close(); err == nil {
		err = cerr
	}
	_ = unlock(h.lock)
	_ = h.lock.Close()
	return err
}

// Close syncs and releases every session this process holds.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	held := s.held
	s.held = map[string]*session{}
	for id, subs := range s.subs {
		for _, ch := range subs {
			close(ch)
		}
		delete(s.subs, id)
	}
	s.mu.Unlock()
	var first error
	for _, h := range held {
		if err := h.close(s); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Append writes one event to its session's chain. The session is taken for
// this process if nobody holds it; ErrHeldElsewhere if another process
// does. As in Postgres, a replay of an event already held is success and a
// different event at a held seq is agent.ErrStepTaken.
func (s *Store) Append(ev agent.Event) error {
	h, err := s.acquire(ev.SessionID, false, nil)
	if err != nil {
		return fmt.Errorf("append event %s/%d: %w", ev.SessionID, ev.Seq, err)
	}
	payload := []byte(ev.Payload)
	if s.redact != nil && len(payload) > 0 {
		if payload = s.redact.Redact(payload); !json.Valid(payload) {
			payload = []byte(`{"withheld":"` + agent.Withheld + `"}`)
		}
	}
	canon, err := canonical(payload)
	if err != nil {
		return fmt.Errorf("append event %s/%d: the payload is not JSON: %w", ev.SessionID, ev.Seq, err)
	}
	ev.Payload = canon
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now().UTC()
	}
	return s.appendHeld(h, ev)
}

// appendHeld writes ev to a held session: one write of the whole line, a
// sync and a new head at every event but a streamed fragment.
func (s *Store) appendHeld(h *session, ev agent.Event) error {
	h.mu.Lock()
	if held, ok := h.seqs[ev.Seq]; ok {
		h.mu.Unlock()
		if held == ev.ID {
			return nil
		}
		return fmt.Errorf("append event %s/%d: %w", ev.SessionID, ev.Seq, agent.ErrStepTaken)
	}
	if ev.Seq <= 0 {
		h.mu.Unlock()
		return fmt.Errorf("append event %s: seq %d is not a step", ev.SessionID, ev.Seq)
	}
	l := lineFor(ev, h.last.Hash)
	raw, err := l.seal()
	if err != nil {
		h.mu.Unlock()
		return err
	}
	if _, err := h.f.Write(append(raw, '\n')); err != nil {
		// A partial write is the torn line the next open cuts off.
		h.mu.Unlock()
		return fmt.Errorf("append event %s/%d: %w", ev.SessionID, ev.Seq, err)
	}
	h.last = Head{Lines: h.last.Lines + 1, Seq: ev.Seq, Hash: l.Hash}
	h.seqs[ev.Seq] = ev.ID
	h.events = insertEvent(h.events, ev)
	var syncErr error
	if boundary(ev.Type) {
		syncErr = h.sync(s)
	}
	ended := s.track(h, ev)
	h.mu.Unlock()

	s.publish(ev)
	if ended && h.child {
		// A subagent's lock goes with its last end; a resume takes it again.
		_ = s.Release(h.id)
	}
	return syncErr
}

// sync makes what was written durable and moves the head to it. The caller
// holds h.mu.
func (h *session) sync(s *Store) error {
	if h.synced == h.last {
		return nil
	}
	if err := syncFile(h.f); err != nil {
		return fmt.Errorf("sync session %s: %w", h.id, err)
	}
	if err := s.writeHead(h.id, h.last); err != nil {
		return err
	}
	h.synced = h.last
	if s.anchor != nil {
		s.anchor(s.tenant, h.id, h.last)
	}
	return nil
}

// Sync makes everything written to session id durable, streamed fragments
// included.
func (s *Store) Sync(id string) error {
	s.mu.Lock()
	h := s.held[id]
	s.mu.Unlock()
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sync(s)
}

// boundary are the events a turn turns on: a prompt, the end of a round
// trip to the model, the end of a run, and what the person does to the
// record itself. Each is synced with the head moved to it, so a machine
// that loses power loses at most the events since the last one. A process
// that dies loses nothing it wrote: the write is whole before it returns.
func boundary(t agent.EventType) bool {
	switch t {
	case agent.EvSessionStarted, agent.EvUserMessage, agent.EvModelCall, agent.EvSessionEnded,
		agent.EvForked, agent.EvFileRestored, agent.EvRecordRepaired, agent.EvSessionBranched,
		agent.EvSessionNamed, agent.EvSessionWoken:
		return true
	}
	return false
}

// track keeps the index in step with what a session's events say: its title,
// name and ends. It reports whether ev closed the session. The caller holds h.mu.
func (s *Store) track(h *session, ev agent.Event) (ended bool) {
	switch ev.Type {
	case agent.EvUserMessage:
		if h.titled {
			return false
		}
		var m agent.Message
		if json.Unmarshal(ev.Payload, &m) == nil && strings.TrimSpace(m.Text) != "" {
			h.titled = true
			_ = s.index.append(indexLine{Op: opTitle, ID: h.id, Title: Title(m.Text)})
		}
	case agent.EvSessionBranched:
		var b agent.SessionBranched
		if json.Unmarshal(ev.Payload, &b) == nil {
			_ = s.index.append(indexLine{Op: opBranch, ID: h.id, Parent: b.From, ForkSeq: b.ThroughSeq})
		}
	case agent.EvSessionNamed:
		var n agent.SessionNamed
		if json.Unmarshal(ev.Payload, &n) == nil {
			_ = s.index.append(indexLine{Op: opName, ID: h.id, Name: n.Name})
		}
	case agent.EvSessionEnded:
		var e agent.SessionEnded
		if json.Unmarshal(ev.Payload, &e) != nil || e.Background > 0 {
			return false
		}
		h.running = false
		_ = s.index.append(indexLine{Op: opEnd, ID: h.id, Ended: string(orReason(e.Reason)),
			HeadLines: h.last.Lines, HeadSeq: h.last.Seq, HeadHash: h.last.Hash})
		return true
	}
	return false
}

func orReason(r agent.TerminalReason) agent.TerminalReason {
	if r == "" {
		return agent.TermCompleted
	}
	return r
}

// Title is how a first prompt labels its session: the first line, cut to 80
// characters. The prompt it is given has already been redacted.
func Title(prompt string) string {
	t := strings.TrimSpace(prompt)
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	if utf8.RuneCountInString(t) > 80 {
		r := []rune(t)
		t = string(r[:79]) + "…"
	}
	return t
}

// systemEvent is an event the store records itself, at the session's next seq.
func (s *Store) systemEvent(h *session, t agent.EventType, payload any) agent.Event {
	raw, _ := json.Marshal(payload)
	canon, _ := canonical(raw)
	h.mu.Lock()
	next := h.last.Seq
	for seq := range h.seqs {
		next = max(next, seq)
	}
	h.mu.Unlock()
	return agent.Event{
		ID: agent.NewEventID(), SessionID: h.id, Seq: next + 1, Type: t,
		Payload: canon, Actor: agent.ActorSystem, Trust: agent.Trusted, CreatedAt: time.Now().UTC(),
	}
}

func (s *Store) publish(ev agent.Event) {
	s.mu.Lock()
	subs := append([]chan agent.Event(nil), s.subs[ev.SessionID]...)
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default: // never block a writer on a slow reader
		}
	}
}

// Subscribe streams the events this process appends to a session.
func (s *Store) Subscribe(sessionID string) <-chan agent.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan agent.Event, 256)
	if s.closed {
		close(ch)
		return ch
	}
	s.subs[sessionID] = append(s.subs[sessionID], ch)
	return ch
}

// Unsubscribe ends a subscription and closes its channel.
func (s *Store) Unsubscribe(sessionID string, ch <-chan agent.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs := s.subs[sessionID]
	for i, c := range subs {
		if c == ch {
			s.subs[sessionID] = append(subs[:i], subs[i+1:]...)
			close(c)
			return
		}
	}
}

// Events returns a session's events in seq order. A session nobody holds
// is read from its file, an unfinished last line left out; nothing is
// written.
func (s *Store) Events(sessionID string) ([]agent.Event, error) {
	return s.Since(sessionID, 0)
}

// Since returns a session's events after seq, in seq order.
func (s *Store) Since(sessionID string, seq int64) ([]agent.Event, error) {
	if err := checkID("session", sessionID); err != nil {
		return nil, nil //nolint:nilerr // no such session is an empty record, as in the other stores
	}
	s.mu.Lock()
	h := s.held[sessionID]
	s.mu.Unlock()
	if h == nil {
		// A read never changes the record: an unfinished last line is left
		// out here and dealt with by the next writer.
		all, _, err := s.readEvents(sessionID)
		return after(all, seq), err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return after(h.events, seq), nil
}

func after(all []agent.Event, seq int64) []agent.Event {
	var out []agent.Event
	for _, e := range all {
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	return out
}

// readEvents reads a session's file: its events in seq order, and whether an
// unfinished last line was left out. A missing file is an empty record.
func (s *Store) readEvents(id string) ([]agent.Event, bool, error) {
	data, err := os.ReadFile(s.Path(id)) // #nosec G304 -- an id checked to be a plain name
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	evs, err := eventsOf(scan(data).raws)
	return evs, len(scan(data).tail) > 0, err
}

// eventsOf parses complete lines into events in seq order.
func eventsOf(raws [][]byte) ([]agent.Event, error) {
	out := make([]agent.Event, 0, len(raws))
	for i, raw := range raws {
		l, err := parseLine(raw)
		if err != nil {
			return nil, fmt.Errorf("the record is damaged at line %d; run abhed record verify: %w", i+1, err)
		}
		out = append(out, l.event())
	}
	sortEvents(out)
	return out, nil
}

func sortEvents(evs []agent.Event) {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Seq < evs[j].Seq })
}

// insertEvent keeps evs in seq order; appends almost always go at the end.
func insertEvent(evs []agent.Event, ev agent.Event) []agent.Event {
	i := len(evs)
	for i > 0 && evs[i-1].Seq > ev.Seq {
		i--
	}
	evs = append(evs, agent.Event{})
	copy(evs[i+1:], evs[i:])
	evs[i] = ev
	return evs
}

// readHead reads session id's head file.
func (s *Store) readHead(id string) (Head, bool) {
	return readHeadFile(s.headPath(id))
}

func readHeadFile(path string) (Head, bool) {
	data, err := os.ReadFile(path) // #nosec G304 -- a path the store builds
	if err != nil {
		return Head{}, false
	}
	var h Head
	if json.Unmarshal(data, &h) != nil {
		return Head{}, false
	}
	return h, true
}

func (s *Store) writeHead(id string, h Head) error {
	return writeHeadFile(s.headPath(id), h)
}

// writeHeadFile replaces a head atomically: a new file renamed over the old.
func writeHeadFile(path string, h Head) error {
	data, err := encode(h)
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'))
}

// writeAtomic writes data to path through a synced temporary file renamed
// into place, owner-only.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// CreateSession starts a session's file and index entry and takes its lock:
// the process that creates a session is its writer until it lets it go.
func (s *Store) CreateSession(_ context.Context, rec store.SessionRecord) error {
	return s.create(rec, "", "")
}

func (s *Store) create(rec store.SessionRecord, parent, kind string) error {
	e := indexLine{
		Cwd: rec.Workspace, Repo: RepoOf(rec.Workspace), GitBranch: BranchOf(rec.Workspace),
		User: rec.User, Model: rec.Model, Mode: rec.Mode, Parent: orStr(parent, rec.ParentID),
		Kind: kind,
	}
	if rec.Prompt != "" {
		p := rec.Prompt
		if s.redact != nil {
			if b, err := json.Marshal(p); err == nil {
				_ = json.Unmarshal(s.redact.Redact(b), &p)
			}
		}
		e.Title = Title(p)
	}
	h, err := s.acquire(rec.ID, true, &e)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.running, h.titled = true, e.Title != ""
	h.mu.Unlock()
	return nil
}

// CreateSubagentSession starts a subagent's own session, named as its
// parent's child so lists leave it out and resume checks whose it is.
func (s *Store) CreateSubagentSession(_ context.Context, id, parentID, description string) error {
	return s.create(store.SessionRecord{ID: id, User: "agent", Model: "subagent", Mode: "auto", Prompt: description}, parentID, kindSubagent)
}

// SubSessionOf reports whether childID is a subagent session parentID started.
func (s *Store) SubSessionOf(_ context.Context, childID, parentID string) (bool, error) {
	e, ok := s.index.get(childID)
	return ok && e.Subagent && e.Parent == parentID, nil
}

// ClaimResume takes a session for this process to continue. It reports
// false when another process holds it, or when this process is still
// running it; a session a crashed process left open is taken, since its
// lock went with the process.
func (s *Store) ClaimResume(_ context.Context, sessionID string) (bool, error) {
	if _, ok := s.index.get(sessionID); !ok {
		if _, err := os.Stat(s.Path(sessionID)); err != nil || checkID("session", sessionID) != nil {
			return false, nil
		}
	}
	s.mu.Lock()
	h := s.held[sessionID]
	s.mu.Unlock()
	if h == nil {
		var err error
		if h, err = s.acquire(sessionID, false, nil); err != nil {
			if errors.Is(err, ErrHeldElsewhere) {
				return false, nil
			}
			return false, err // an unverified record is refused, and says why
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false, nil
	}
	h.running = true
	_ = s.index.append(indexLine{Op: opOpen, ID: sessionID})
	return true, nil
}

// Unclaim hands back a claim that wrote nothing that ends a run, such as a
// fork or a rename between tasks: the session is at rest again, still held.
func (s *Store) Unclaim(sessionID string) {
	s.mu.Lock()
	h := s.held[sessionID]
	s.mu.Unlock()
	if h != nil {
		h.mu.Lock()
		h.running = false
		h.mu.Unlock()
	}
}

// ListSessions lists recent top-level sessions as session rows, for the
// commands that were written for Postgres.
func (s *Store) ListSessions(_ context.Context, limit int) ([]store.SessionRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	entries, err := s.index.list(Filter{All: true, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]store.SessionRecord, 0, len(entries))
	for _, e := range entries {
		out = append(out, s.row(e))
	}
	return out, nil
}

// GetSession returns a session's row. It is ended unless a process is
// running it now: the lock, not the last event, says whether it is alive.
func (s *Store) GetSession(_ context.Context, id string) (store.SessionRecord, error) {
	e, ok := s.index.get(id)
	if !ok || e.Pruned {
		return store.SessionRecord{}, store.ErrNotFound
	}
	return s.row(e), nil
}

func (s *Store) row(e Entry) store.SessionRecord {
	rec := store.SessionRecord{
		ID: e.ID, Tenant: s.tenant, User: e.User, Workspace: e.Cwd,
		Prompt: e.Title, ParentID: e.Parent, StartedAt: e.Created, TerminalReason: e.Ended,
	}
	if !s.running(e.ID) {
		at := e.Updated
		rec.EndedAt = &at
		if rec.TerminalReason == "" {
			rec.TerminalReason = "lost"
		}
	} else {
		rec.TerminalReason = ""
	}
	return rec
}

// running reports whether a process is running session id now: this one,
// between a claim and its end, or another holding its lock.
func (s *Store) running(id string) bool {
	s.mu.Lock()
	h := s.held[id]
	s.mu.Unlock()
	if h != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.running
	}
	return s.heldElsewhere(id)
}

// heldElsewhere reports whether another process holds session id's lock.
func (s *Store) heldElsewhere(id string) bool {
	lk, err := os.Open(s.lockPath(id)) // #nosec G304 -- an id checked to be a plain name
	if err != nil {
		return false
	}
	defer func() { _ = lk.Close() }()
	got, err := tryLock(lk)
	if err != nil {
		return false
	}
	if got {
		_ = unlock(lk)
		return false
	}
	return true
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
