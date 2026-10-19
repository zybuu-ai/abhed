package ocsf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/filelock"
)

// sinkQueue is how many lines wait in memory before the rest are read back
// from the record.
var sinkQueue = 4096

// Cursor is how far a sink has written one session: the last seq, and Pos,
// where the record's reader goes on from (bytes into the local record's file;
// zero where the store reads by seq alone). Provider is what the mapper needs
// from before the cursor, kept so it is never read again; Primed says it was
// looked for.
type Cursor struct {
	Seq      int64  `json:"seq"`
	Pos      int64  `json:"pos,omitempty"`
	Provider string `json:"provider,omitempty"`
	Primed   bool   `json:"primed,omitempty"`
}

// ErrStandby is a CursorStore's answer when another process holds the right
// to write: this sink then writes nothing until it holds it.
var ErrStandby = errors.New("another process writes the OCSF file")

// CursorStore keeps a sink's cursors and decides which process writes.
type CursorStore interface {
	// Lock is held while lines are written and the cursors moved; it is
	// ErrStandby while another process is the writer.
	Lock() (unlock func(), err error)
	// Load returns the cursors, and whether any were ever saved.
	Load() (map[string]Cursor, bool, error)
	// Save sets the cursors of the sessions it names.
	Save(map[string]Cursor) error
}

// SinkOptions configure a JSONL file of OCSF events.
type SinkOptions struct {
	// Path is the file; rotated copies are Path.1, Path.2 and so on.
	Path string
	// Version is the schema version written; "" is Version.
	Version string
	// MaxBytes rotates the file once it would pass this size; 0 is 64 MiB.
	MaxBytes int64
	// Keep is how many rotated files are kept; 0 is 5.
	Keep int
	// Map names the source of every event.
	Map Options
	// Cursors keeps the cursors; nil keeps them in Path.cursor under a lock
	// on Path.lock, for the processes of one machine.
	Cursors CursorStore
	// Read gives a session's lines after a cursor, each with its Pos where
	// the store has one, for the lines the sink did not see written.
	Read func(session string, from Cursor) ([]Line, error)
	// Sessions gives where each of the record's sessions ends (Seq, and Pos
	// where the reader has one), so the lines past a cursor are found.
	Sessions func() (map[string]Cursor, error)
	// Changed is Sessions for the sessions written lately, which a poll asks;
	// nil asks Sessions.
	Changed func() (map[string]Cursor, error)
	// Poll, when set, looks for new lines this often, for a record the sink
	// is not handed lines from.
	Poll time.Duration
	// Feed, when set, is called at each poll while this process writes, to
	// hand the sink the lines written since the last (through add, as Add);
	// a poll then lists no sessions, which only a new writer's first scan does.
	Feed func(add func(Line)) error
}

// Sink writes the record's lines to a JSONL file as OCSF events. Its cursor,
// the last line written of each session, survives a restart: what was
// recorded while no sink ran is written when one next runs, read from the
// cursor on, and a line already written is not written again by any process
// sharing the cursors. Delivery is at least once for a sink that stops
// between a write and its cursor; metadata.uid tells a repeat.
type Sink struct {
	o      SinkOptions
	in     chan Line
	wake   chan struct{}
	done   chan struct{}
	mapper *Mapper

	mu      sync.Mutex
	behind  map[string]bool // sessions read from the record, not the queue
	closed  bool
	closing sync.Once
	err     error
	writer  bool // this process held the right to write at its last try
}

// OpenSink starts a sink. Sessions with no cursor yet when the first is
// saved start at their present end, so turning the sink on does not write
// the whole history; `abhed logs` covers that.
func OpenSink(o SinkOptions) (*Sink, error) {
	if o.Path == "" {
		return nil, errors.New("ocsf: the sink has no path")
	}
	if o.Version == "" {
		o.Version = Version
	}
	if err := CheckVersion(o.Version); err != nil {
		return nil, err
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 64 << 20
	}
	if o.Keep <= 0 {
		o.Keep = 5
	}
	if err := os.MkdirAll(filepath.Dir(o.Path), 0o700); err != nil {
		return nil, fmt.Errorf("ocsf: the sink's folder: %w", err)
	}
	if o.Cursors == nil {
		o.Cursors = &fileCursors{path: o.Path}
	}
	s := &Sink{o: o, in: make(chan Line, sinkQueue), wake: make(chan struct{}, 1), done: make(chan struct{}),
		mapper: NewMapper(o.Map), behind: map[string]bool{}}
	if err := s.scan(true); err != nil && !errors.Is(err, ErrStandby) {
		return nil, err
	}
	go s.run()
	return s, nil
}

// scan finds the sessions with lines past their cursor. The first scan after
// this process becomes the writer looks at every session; a later poll at
// those written lately.
func (s *Sink) scan(first bool) error {
	unlock, err := s.o.Cursors.Lock()
	if err != nil {
		s.mu.Lock()
		s.writer = false
		s.mu.Unlock()
		return err
	}
	defer unlock()
	s.mu.Lock()
	full := first || !s.writer
	s.writer = true
	s.mu.Unlock()
	cur, existed, err := s.o.Cursors.Load()
	if err != nil {
		return err
	}
	if !full && s.o.Feed != nil {
		return nil
	}
	list := s.o.Sessions
	if !full && s.o.Changed != nil {
		list = s.o.Changed
	}
	if list == nil {
		if !existed {
			return s.o.Cursors.Save(map[string]Cursor{})
		}
		return nil
	}
	ends, err := list()
	if err != nil {
		return fmt.Errorf("ocsf: list the record's sessions: %w", err)
	}
	if !existed {
		// Saved even when empty, so the next start knows the sink has begun.
		return s.o.Cursors.Save(ends)
	}
	var behind []string
	for id, end := range ends {
		if c, ok := cur[id]; !ok || past(end, c) {
			behind = append(behind, id)
		}
	}
	if len(behind) > 0 {
		s.mu.Lock()
		for _, id := range behind {
			s.behind[id] = true
		}
		s.mu.Unlock()
		s.poke()
	}
	return nil
}

// past reports whether a session's end is beyond cursor c.
func past(end, c Cursor) bool {
	if end.Pos > 0 {
		return end.Pos > c.Pos
	}
	return end.Seq > c.Seq
}

// Add queues a line the record just wrote. It never blocks: a line that
// does not fit is read back from the record.
func (s *Sink) Add(l Line) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.in <- l:
	default:
		s.behind[l.SessionID] = true
		s.poke()
	}
}

func (s *Sink) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Close writes what is queued and stops the sink.
func (s *Sink) Close() error {
	s.closing.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.in)
		s.mu.Unlock()
		<-s.done
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Err is the last error the sink had writing; it goes on after one.
func (s *Sink) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Sink) run() {
	defer close(s.done)
	var tick <-chan time.Time
	if s.o.Poll > 0 {
		t := time.NewTicker(s.o.Poll)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case l, ok := <-s.in:
			if !ok {
				s.catchUp()
				return
			}
			batch := []Line{l}
		drain:
			for len(batch) < 512 {
				select {
				case l, ok := <-s.in:
					if !ok {
						break drain
					}
					batch = append(batch, l)
				default:
					break drain
				}
			}
			s.write(batch, false)
		case <-s.wake:
			s.catchUp()
		case <-tick:
			err := s.scan(false)
			if err != nil && !errors.Is(err, ErrStandby) {
				s.fail(err)
			}
			if err == nil && s.o.Feed != nil {
				if err := s.o.Feed(s.Add); err != nil {
					s.fail(err)
				}
			}
			s.catchUp()
		}
	}
}

// catchUp writes, from the record, the sessions the queue did not carry.
func (s *Sink) catchUp() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.behind))
	for id := range s.behind {
		ids = append(ids, id)
	}
	s.behind = map[string]bool{}
	s.mu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		if s.o.Read == nil {
			continue
		}
		cur, err := s.cursorOf(id)
		if errors.Is(err, ErrStandby) {
			return
		}
		if err != nil {
			s.fail(err)
			continue
		}
		lines, err := s.o.Read(id, cur)
		if err != nil {
			s.fail(fmt.Errorf("ocsf: read session %s: %w", id, err))
			continue
		}
		s.write(lines, true)
	}
	// Drained here too, so a close writes what was still queued.
	var rest []Line
	for more := true; more; {
		select {
		case l, ok := <-s.in:
			if ok {
				rest = append(rest, l)
			} else {
				more = false
			}
		default:
			more = false
		}
	}
	s.write(rest, false)
}

func (s *Sink) cursorOf(id string) (Cursor, error) {
	unlock, err := s.o.Cursors.Lock()
	if err != nil {
		return Cursor{}, err
	}
	defer unlock()
	cur, _, err := s.o.Cursors.Load()
	return cur[id], err
}

// write maps and appends lines past each session's cursor, then moves it.
func (s *Sink) write(lines []Line, fromRecord bool) {
	if len(lines) == 0 {
		return
	}
	unlock, err := s.o.Cursors.Lock()
	if errors.Is(err, ErrStandby) {
		return
	}
	if err != nil {
		s.fail(err)
		return
	}
	defer unlock()
	cur, _, err := s.o.Cursors.Load()
	if err != nil {
		s.fail(err)
		return
	}
	s.mu.Lock()
	behind := map[string]bool{}
	for id := range s.behind {
		behind[id] = true
	}
	s.mu.Unlock()
	var buf bytes.Buffer
	moved := map[string]Cursor{}
	for _, l := range lines {
		c := cur[l.SessionID]
		if l.Seq <= c.Seq {
			continue
		}
		// A session that lost lines from the queue is written from the
		// record, in order, before any queued line of it.
		if !fromRecord && behind[l.SessionID] {
			continue
		}
		c = s.prime(l.SessionID, c)
		ev, ok, err := s.mapper.Map(l)
		c.Seq, c.Provider = l.Seq, s.mapper.provider[l.SessionID]
		if l.Pos > 0 {
			c.Pos = l.Pos
		}
		cur[l.SessionID], moved[l.SessionID] = c, c
		if err != nil {
			s.fail(fmt.Errorf("ocsf: map %s: %w", l, err))
			continue
		}
		if !ok {
			continue
		}
		if s.o.Version != Version {
			if ev, err = Downgrade(ev, s.o.Version); err != nil {
				s.fail(err)
				continue
			}
		}
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(ev); err != nil {
			s.fail(err)
			continue
		}
	}
	// Lines that map to nothing, such as reply fragments, move no cursor on
	// their own: read again after a restart, they are left out again.
	if buf.Len() == 0 {
		// Read from the record, they move it, so the next catch-up reads past them.
		if fromRecord && len(moved) > 0 {
			if err := s.o.Cursors.Save(moved); err != nil {
				s.fail(err)
			}
		}
		return
	}
	if err := s.append(buf.Bytes()); err != nil {
		// The cursor stays: the lines are written again from the record.
		s.fail(err)
		s.mu.Lock()
		for _, l := range lines {
			s.behind[l.SessionID] = true
		}
		s.mu.Unlock()
		s.poke()
		return
	}
	if err := s.o.Cursors.Save(moved); err != nil {
		s.fail(err)
	}
}

// prime gives the mapper what a session's lines before its cursor said: the
// provider the cursor kept, or, once for a cursor that never kept one, what
// the record's lines up to it say.
func (s *Sink) prime(id string, c Cursor) Cursor {
	if _, ok := s.mapper.provider[id]; ok {
		return c
	}
	if c.Provider != "" {
		s.mapper.provider[id] = c.Provider
		return c
	}
	if c.Seq <= 0 || c.Primed || s.o.Read == nil {
		c.Primed = true
		return c
	}
	c.Primed = true
	lines, err := s.o.Read(id, Cursor{})
	if err != nil {
		return c
	}
	for _, l := range lines {
		if l.Seq > c.Seq {
			break
		}
		s.mapper.Observe(l)
	}
	return c
}

func (s *Sink) append(data []byte) error {
	if info, err := os.Stat(s.o.Path); err == nil && info.Size() > 0 && info.Size()+int64(len(data)) > s.o.MaxBytes {
		if err := s.rotate(); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(s.o.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("ocsf: open the sink: %w", err)
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	return errors.Join(werr, serr, cerr)
}

// rotate moves the file to Path.1, each older copy one along, dropping the last.
func (s *Sink) rotate() error {
	_ = os.Remove(s.o.Path + "." + strconv.Itoa(s.o.Keep))
	for i := s.o.Keep - 1; i >= 1; i-- {
		from := s.o.Path + "." + strconv.Itoa(i)
		if _, err := os.Stat(from); err == nil {
			if err := os.Rename(from, s.o.Path+"."+strconv.Itoa(i+1)); err != nil {
				return fmt.Errorf("ocsf: rotate the sink: %w", err)
			}
		}
	}
	if err := os.Rename(s.o.Path, s.o.Path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("ocsf: rotate the sink: %w", err)
	}
	return nil
}

func (s *Sink) fail(err error) {
	s.mu.Lock()
	first := s.err == nil
	s.err = err
	s.mu.Unlock()
	if first {
		slog.Warn("ocsf: the OCSF file could not be written; it catches up from the record", "err", err)
	}
}

// fileCursors keeps the cursors in Path.cursor, under a lock on Path.lock
// that the processes of one machine share.
type fileCursors struct {
	path string
}

func (f *fileCursors) Lock() (func(), error) {
	unlock, err := filelock.Lock(f.path+".lock", 10*time.Second, "another Abhed process is writing the OCSF file")
	if err != nil {
		return nil, fmt.Errorf("ocsf: %w", err)
	}
	return unlock, nil
}

type cursorFile struct {
	Version  int               `json:"version"`
	Sessions map[string]Cursor `json:"sessions"`
}

func (f *fileCursors) Load() (map[string]Cursor, bool, error) {
	cur := cursorFile{Sessions: map[string]Cursor{}}
	data, err := os.ReadFile(f.path + ".cursor")
	if errors.Is(err, os.ErrNotExist) {
		return cur.Sessions, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("ocsf: read the sink's cursor: %w", err)
	}
	if err := json.Unmarshal(data, &cur); err != nil {
		return nil, false, fmt.Errorf("ocsf: the sink's cursor %s is damaged: %w", f.path+".cursor", err)
	}
	if cur.Sessions == nil {
		cur.Sessions = map[string]Cursor{}
	}
	return cur.Sessions, true, nil
}

func (f *fileCursors) Save(moved map[string]Cursor) error {
	all, _, err := f.Load()
	if err != nil {
		return err
	}
	for id, c := range moved {
		all[id] = c
	}
	data, err := json.Marshal(cursorFile{Version: 2, Sessions: all})
	if err != nil {
		return err
	}
	tmp := f.path + ".cursor.tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("ocsf: write the sink's cursor: %w", err)
	}
	if err := os.Rename(tmp, f.path+".cursor"); err != nil {
		return fmt.Errorf("ocsf: write the sink's cursor: %w", err)
	}
	return nil
}
