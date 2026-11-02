package recordship

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/filelock"
	"github.com/zybuu-ai/abhed/store/local"
)

// Doer sends one request to the org server with the sign-in token and
// returns its status and body (at most 1 MiB of it).
type Doer func(ctx context.Context, method, path, contentType string, body []byte) (int, []byte, error)

// Options make a Shipper.
type Options struct {
	Store *local.Store
	Do    Doer
	// URL is the server shipped to; a state kept for another starts over.
	URL string
	// StatePath is where the cursors are kept, under Abhed's state.
	StatePath string
	// Redact is the record's redactor: a line it would still change is not sent.
	Redact agent.Redactor
	// MaxBacklog is the unshipped bytes past which a backlog event is raised.
	MaxBacklog int64
	Host       string
	// OnEvent, when set, is told each event as it is raised.
	OnEvent func(Event)
	// Clock is the time; nil is the wall clock.
	Clock func() time.Time
}

// Cursor is how far one session has reached the server.
type Cursor struct {
	Lines int64  `json:"lines"`
	Hash  string `json:"hash"`
	// Offset is the record file's offset just past line Lines; Size the
	// file's size when last read.
	Offset int64 `json:"offset"`
	Size   int64 `json:"size"`
	// Blocked says why the session is no longer shipped.
	Blocked string `json:"blocked,omitempty"`
}

// State is the shipper's file.
type State struct {
	V   int    `json:"v"`
	URL string `json:"url"`
	// Since is when shipping began: sessions created before are not sent.
	Since    time.Time         `json:"since"`
	Sessions map[string]Cursor `json:"sessions"`
	// Events are the latest events, newest last, at most 100.
	Events []Logged `json:"events,omitempty"`
	// LastOK and LastError are the last pass that reached the server, and
	// why the last one that did not failed.
	LastOK    time.Time `json:"last_ok,omitzero"`
	LastError string    `json:"last_error,omitempty"`
	Backlog   int64     `json:"backlog"`
}

// Logged is an event as the state keeps it; Sent marks one the server was given.
type Logged struct {
	Event
	Sent bool `json:"sent,omitempty"`
}

// Shipper sends the record to the server.
type Shipper struct {
	o     Options
	nudge chan struct{}
	mu    sync.Mutex
	st    *State
	// failing is set after a pass that did not reach the server.
	failing  bool
	overflow bool
}

// New loads or starts the shipper's state.
func New(o Options) (*Shipper, error) {
	if o.Store == nil || o.Do == nil || o.StatePath == "" || o.URL == "" {
		return nil, errors.New("recordship: a store, a doer, a state path and a URL are needed")
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Host == "" {
		o.Host, _ = os.Hostname()
	}
	if o.MaxBacklog <= 0 {
		o.MaxBacklog = 512 << 20
	}
	st, err := LoadState(o.StatePath)
	if err != nil {
		return nil, err
	}
	if st == nil || st.URL != o.URL {
		st = &State{V: 1, URL: o.URL, Since: o.Clock().UTC(), Sessions: map[string]Cursor{}}
		if err := SaveState(o.StatePath, st); err != nil {
			return nil, err
		}
	}
	return &Shipper{o: o, st: st, nudge: make(chan struct{}, 1)}, nil
}

// LoadState reads the state file; a missing one is (nil, nil).
func LoadState(path string) (*State, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the shipper's own state file
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if st.Sessions == nil {
		st.Sessions = map[string]Cursor{}
	}
	return &st, nil
}

// SaveState writes st 0600 in one step.
func SaveState(path string, st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Nudge asks for a pass soon; it never blocks.
func (s *Shipper) Nudge() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// Status is a copy of the state.
func (s *Shipper) Status() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := *s.st
	st.Sessions = make(map[string]Cursor, len(s.st.Sessions))
	for k, v := range s.st.Sessions {
		st.Sessions[k] = v
	}
	st.Events = slices.Clone(s.st.Events)
	return st
}

// Run makes passes until ctx ends: when nudged, and at least every poll;
// after a failure it waits longer each time, up to five minutes.
func (s *Shipper) Run(ctx context.Context, poll time.Duration) {
	if poll <= 0 {
		poll = 10 * time.Second
	}
	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.nudge:
		case <-time.After(max(wait, poll)):
		}
		if err := s.Pass(ctx); err != nil && ctx.Err() == nil {
			wait = min(max(2*wait, poll), 5*time.Minute)
		} else {
			wait = 0
		}
	}
}

// ErrBusy is a pass skipped because another process is shipping.
var ErrBusy = errors.New("another Abhed process is shipping the record")

// Pass ships what has not reached the server; a session it had to stop is an
// event, and only failing to reach the server is an error.
func (s *Shipper) Pass(ctx context.Context) error {
	unlock, err := filelock.Lock(s.o.StatePath+".lock", 0, "%s is held (waited %s)")
	if err != nil {
		return ErrBusy
	}
	defer unlock()
	// Another process may have moved the cursors since this one last read them.
	if st, err := LoadState(s.o.StatePath); err == nil && st != nil && st.URL == s.o.URL {
		s.mu.Lock()
		s.st = st
		s.mu.Unlock()
	}
	ids, err := s.o.Store.SessionIDs()
	if err != nil {
		return err
	}
	listed := map[string]bool{}
	var passErr error
	var backlog int64
	for _, id := range ids {
		listed[id] = true
		e, err := s.o.Store.Index().Get(id)
		if err != nil || e.Created.Before(s.since()) {
			continue
		}
		if passErr == nil {
			passErr = s.ship(ctx, id)
		}
		size, _ := s.o.Store.Size(id)
		c := s.cursor(id)
		if c.Blocked == "" && size > c.Offset {
			backlog += size - c.Offset
		}
		if size > c.Size {
			// Kept even when nothing reached the server, so a prune before it does shows.
			c.Size = size
			s.setCursor(id, c)
		}
	}
	s.mu.Lock()
	var gone []string
	for id, c := range s.st.Sessions {
		if !listed[id] {
			gone = append(gone, id)
			if c.Blocked == "" && c.Offset < c.Size {
				s.eventLocked(Event{Kind: KindGap, Session: id, Lines: c.Lines,
					Detail: "the session was pruned before all of it was shipped"})
			}
		}
	}
	for _, id := range gone {
		delete(s.st.Sessions, id)
	}
	s.st.Backlog = backlog
	over := backlog > s.o.MaxBacklog
	if over && !s.overflow {
		s.eventLocked(Event{Kind: KindBacklog, Detail: fmt.Sprintf("%d bytes of the record wait to be shipped, over the limit of %d", backlog, s.o.MaxBacklog)})
	}
	s.overflow = over
	now := s.o.Clock().UTC()
	if passErr == nil {
		if s.failing {
			s.eventLocked(Event{Kind: KindResumed, Detail: "the record reaches the server again"})
		}
		s.failing, s.st.LastOK, s.st.LastError = false, now, ""
	} else {
		s.failing, s.st.LastError = true, passErr.Error()
	}
	err = SaveState(s.o.StatePath, s.st)
	events := s.unsent()
	s.mu.Unlock()
	if passErr == nil && len(events) > 0 && s.report(ctx, events) {
		s.mu.Lock()
		for i := range s.st.Events {
			s.st.Events[i].Sent = true
		}
		err = errors.Join(err, SaveState(s.o.StatePath, s.st))
		s.mu.Unlock()
	}
	if passErr != nil {
		return passErr
	}
	return err
}

func (s *Shipper) since() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Since
}

func (s *Shipper) cursor(id string) Cursor {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.st.Sessions[id]
	if !ok {
		c = Cursor{Hash: local.Genesis}
	}
	return c
}

func (s *Shipper) setCursor(id string, c Cursor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Sessions[id] = c
}

func (s *Shipper) block(id string, c Cursor, kind, why string) {
	c.Blocked = why
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Sessions[id] = c
	s.eventLocked(Event{Kind: kind, Session: id, Lines: c.Lines, Detail: why})
}

// eventLocked keeps an event and tells OnEvent; s.mu is held.
func (s *Shipper) eventLocked(e Event) {
	e.At, e.Host = s.o.Clock().UTC(), s.o.Host
	s.st.Events = append(s.st.Events, Logged{Event: e})
	if n := len(s.st.Events); n > 100 {
		s.st.Events = slices.Clone(s.st.Events[n-100:])
	}
	if s.o.OnEvent != nil {
		s.o.OnEvent(e)
	}
}

// unsent are the events not yet reported; s.mu is held.
func (s *Shipper) unsent() []Event {
	var out []Event
	for i := range s.st.Events {
		if !s.st.Events[i].Sent {
			out = append(out, s.st.Events[i].Event)
		}
	}
	return out
}

// report sends events to the server and reports whether it took them.
func (s *Shipper) report(ctx context.Context, events []Event) bool {
	b, err := json.Marshal(map[string]any{"events": events})
	if err != nil {
		return false
	}
	status, _, err := s.o.Do(ctx, http.MethodPost, EventsPath, "application/json", b)
	return err == nil && status/100 == 2
}

// ship sends one session's lines past its cursor, batch by batch.
func (s *Shipper) ship(ctx context.Context, id string) error {
	c := s.cursor(id)
	if c.Blocked != "" {
		return nil
	}
	size, err := s.o.Store.Size(id)
	if err != nil || size == 0 || (size == c.Offset && c.Lines > 0) {
		return err
	}
	lines, err := s.pending(id, &c)
	if err != nil || c.Blocked != "" {
		return err
	}
	c.Size = size
	moves := 0
	for len(lines) > 0 {
		var raws [][]byte
		n := 0
		for _, l := range lines {
			if n > 0 && (n+len(l.Raw)+1 > MaxBatchBytes-4096 || len(raws) >= MaxBatchLines) {
				break
			}
			if len(l.Raw) > MaxLineBytes {
				if len(raws) == 0 {
					s.block(id, c, KindOversize, fmt.Sprintf("line %d is %d bytes, over the %d a line may be; it and what follows are not shipped", c.Lines+1, len(l.Raw), MaxLineBytes))
					return nil
				}
				break
			}
			if s.o.Redact != nil && !bytes.Equal(s.o.Redact.Redact(l.Payload), l.Payload) {
				if len(raws) == 0 {
					s.block(id, c, KindBlocked, fmt.Sprintf("line %d holds a value the secrets store redacts; it and what follows are not shipped", c.Lines+1))
					return nil
				}
				break
			}
			raws = append(raws, l.Raw)
			n += len(l.Raw) + 1
		}
		next, err := s.send(ctx, id, &c, lines[:len(raws)])
		if err != nil || c.Blocked != "" {
			return err
		}
		if next != nil {
			// The server holds a different place: read again from it, a few times a pass.
			if moves++; moves > 3 {
				return fmt.Errorf("the org server moved its place in session %s %d times in one pass", id, moves)
			}
			lines = next
			continue
		}
		lines = lines[len(raws):]
		s.setCursor(id, c)
	}
	s.setCursor(id, c)
	return nil
}

// pending are the lines after c, which it re-reads from the start of the
// file when its offset no longer falls where it should.
func (s *Shipper) pending(id string, c *Cursor) ([]local.Sealed, error) {
	lines, _, err := s.o.Store.LinesFrom(id, c.Offset)
	if err != nil {
		return nil, err
	}
	if len(lines) > 0 && lines[0].Prev == c.Hash && (c.Offset > 0 || c.Lines == 0) {
		return lines, nil
	}
	if len(lines) == 0 && c.Offset > 0 {
		return nil, nil
	}
	return s.from(id, c, c.Lines, c.Hash)
}

// from re-reads the session, checks line n hashes to hash and returns the
// lines after it with c moved there; a record that no longer holds it blocks.
func (s *Shipper) from(id string, c *Cursor, n int64, hash string) ([]local.Sealed, error) {
	all, _, err := s.o.Store.LinesFrom(id, 0)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		c.Lines, c.Hash, c.Offset = 0, local.Genesis, 0
		return all, nil
	}
	if int64(len(all)) < n || all[n-1].Hash != hash {
		s.block(id, *c, KindDiverged, fmt.Sprintf("the local record no longer holds line %d as it was shipped", n))
		c.Blocked = "diverged"
		return nil, nil
	}
	c.Lines, c.Hash, c.Offset = n, hash, all[n-1].End
	return all[n:], nil
}

// send posts one batch. It returns the lines to send next when the server
// holds another place than c, which it moves c to.
func (s *Shipper) send(ctx context.Context, id string, c *Cursor, batch []local.Sealed) ([]local.Sealed, error) {
	raws := make([][]byte, len(batch))
	for i, l := range batch {
		raws[i] = l.Raw
	}
	body, err := EncodeBatch(Header{Session: id, From: c.Lines, Before: c.Hash, Host: s.o.Host}, raws)
	if err != nil {
		return nil, err
	}
	status, resp, err := s.o.Do(ctx, http.MethodPost, LinesPath+id+"/lines", ContentType, body)
	if err != nil {
		return nil, fmt.Errorf("the org server cannot be reached: %w", err)
	}
	var ack Ack
	_ = json.Unmarshal(resp, &ack)
	last := batch[len(batch)-1]
	switch status {
	case http.StatusOK:
		if ack.Lines != c.Lines+int64(len(batch)) || ack.Hash != last.Hash {
			return nil, fmt.Errorf("the org server's answer (line %d) does not match what was sent (line %d)", ack.Lines, c.Lines+int64(len(batch)))
		}
		c.Lines, c.Hash, c.Offset = ack.Lines, ack.Hash, last.End
		return nil, nil
	case http.StatusConflict:
		if ack.Lines < 0 || (ack.Lines == c.Lines && ack.Hash == c.Hash) {
			return nil, fmt.Errorf("the org server refused session %s's batch: %s", id, ack.Error)
		}
		return s.from(id, c, ack.Lines, ack.Hash)
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("the org server refused the sign-in (%d); run abhed login again", status)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		s.block(id, *c, KindRefused, "the org server refused the record: "+ack.Error)
		c.Blocked = "refused"
		return nil, nil
	}
	return nil, fmt.Errorf("the org server answered %d", status)
}
