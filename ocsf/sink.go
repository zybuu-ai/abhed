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
	// Read gives a session's lines after seq from the record, for the lines
	// the sink did not see written: before it started, or while it was behind.
	Read func(session string, after int64) ([]Line, error)
	// Sessions lists the record's sessions with their last seq, so lines
	// written while no sink ran are caught up when one starts.
	Sessions func() (map[string]int64, error)
}

// Sink writes the record's lines to a JSONL file as OCSF events. Its cursor,
// the last seq written of each session, is kept beside the file and survives
// a restart: what was recorded while no sink ran is written when one next
// starts, and a line already written is never written twice by the sinks of
// processes sharing the file. Delivery is at least once for a sink that
// crashes between a write and its cursor; metadata.uid tells a repeat.
type Sink struct {
	o      SinkOptions
	in     chan Line
	wake   chan struct{}
	done   chan struct{}
	mapper *Mapper

	mu      sync.Mutex
	behind  map[string]bool // sessions whose lines overflowed the queue
	primed  map[string]bool
	closed  bool
	closing sync.Once
	err     error
}

// cursorFile is the sink's progress, by session.
type cursorFile struct {
	Version  int              `json:"version"`
	Sessions map[string]int64 `json:"sessions"`
}

// OpenSink starts a sink. A file with no cursor yet starts at each session's
// present end, so turning the sink on does not write the whole history; the
// lines of `abhed logs` cover that.
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
	s := &Sink{o: o, in: make(chan Line, sinkQueue), wake: make(chan struct{}, 1), done: make(chan struct{}),
		mapper: NewMapper(o.Map), behind: map[string]bool{}, primed: map[string]bool{}}
	if err := s.start(); err != nil {
		return nil, err
	}
	go s.run()
	return s, nil
}

// start writes a first cursor, or marks what was recorded since the last.
func (s *Sink) start() error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	cur, existed, err := s.readCursor()
	if err != nil {
		return err
	}
	var heads map[string]int64
	if s.o.Sessions != nil {
		if heads, err = s.o.Sessions(); err != nil {
			return fmt.Errorf("ocsf: list the record's sessions: %w", err)
		}
	}
	if !existed {
		for id, seq := range heads {
			cur.Sessions[id] = seq
		}
		return s.writeCursor(cur)
	}
	for id, seq := range heads {
		if seq > cur.Sessions[id] {
			s.behind[id] = true
		}
	}
	if len(s.behind) > 0 {
		s.poke()
	}
	return nil
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
	for {
		var batch []Line
		select {
		case l, ok := <-s.in:
			if !ok {
				s.catchUp()
				return
			}
			batch = append(batch, l)
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
		}
	}
}

// catchUp writes, from the record, the sessions the queue lost lines of.
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
		cur, _, err := s.lockedCursor()
		if err != nil {
			s.fail(err)
			continue
		}
		lines, err := s.o.Read(id, cur.Sessions[id])
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

func (s *Sink) lockedCursor() (cursorFile, bool, error) {
	unlock, err := s.lock()
	if err != nil {
		return cursorFile{}, false, err
	}
	defer unlock()
	return s.readCursor()
}

// write maps and appends lines past each session's cursor, then moves it.
func (s *Sink) write(lines []Line, fromRecord bool) {
	if len(lines) == 0 {
		return
	}
	unlock, err := s.lock()
	if err != nil {
		s.fail(err)
		return
	}
	defer unlock()
	cur, _, err := s.readCursor()
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
	for _, l := range lines {
		if l.Seq <= cur.Sessions[l.SessionID] {
			continue
		}
		// A session that lost lines from the queue is written from the
		// record, in order, before any queued line of it.
		if !fromRecord && behind[l.SessionID] {
			continue
		}
		s.prime(l.SessionID, cur.Sessions[l.SessionID])
		ev, ok, err := s.mapper.Map(l)
		cur.Sessions[l.SessionID] = l.Seq
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
	if err := s.writeCursor(cur); err != nil {
		s.fail(err)
	}
}

// prime gives the mapper what a session's lines before seq say, once.
func (s *Sink) prime(id string, upTo int64) {
	if s.primed[id] {
		return
	}
	s.primed[id] = true
	if upTo <= 0 || s.o.Read == nil {
		return
	}
	lines, err := s.o.Read(id, 0)
	if err != nil {
		return
	}
	for _, l := range lines {
		if l.Seq > upTo {
			break
		}
		s.mapper.Observe(l)
	}
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

func (s *Sink) lock() (func(), error) {
	unlock, err := filelock.Lock(s.o.Path+".lock", 10*time.Second, "another Abhed process is writing the OCSF file")
	if err != nil {
		return nil, fmt.Errorf("ocsf: %w", err)
	}
	return unlock, nil
}

func (s *Sink) readCursor() (cursorFile, bool, error) {
	cur := cursorFile{Version: 1, Sessions: map[string]int64{}}
	data, err := os.ReadFile(s.o.Path + ".cursor")
	if errors.Is(err, os.ErrNotExist) {
		return cur, false, nil
	}
	if err != nil {
		return cur, false, fmt.Errorf("ocsf: read the sink's cursor: %w", err)
	}
	if err := json.Unmarshal(data, &cur); err != nil {
		return cur, false, fmt.Errorf("ocsf: the sink's cursor %s is damaged: %w", s.o.Path+".cursor", err)
	}
	if cur.Sessions == nil {
		cur.Sessions = map[string]int64{}
	}
	return cur, true, nil
}

func (s *Sink) writeCursor(cur cursorFile) error {
	data, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	tmp := s.o.Path + ".cursor.tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("ocsf: write the sink's cursor: %w", err)
	}
	if err := os.Rename(tmp, s.o.Path+".cursor"); err != nil {
		return fmt.Errorf("ocsf: write the sink's cursor: %w", err)
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
