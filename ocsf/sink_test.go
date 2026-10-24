package ocsf_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/ocsf"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// reads are the cursors the sinks' catch-ups read from.
var (
	readsMu sync.Mutex
	reads   []ocsf.Cursor
)

// canary redacts one stored secret, as the vault's redactor does.
type canary struct{}

func (canary) Redact(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("sk-CANARY-4242"), []byte("[REDACTED:api_key]"))
}
func (canary) Span() int { return len("sk-CANARY-4242") }

// recordWithSink opens a record whose lines feed a sink, as the CLI does.
func recordWithSink(t *testing.T, dir, path string) (*local.Store, *ocsf.Sink) {
	t.Helper()
	var sink *ocsf.Sink
	rec, err := local.Open(local.Options{Dir: dir, User: "tester", Redact: canary{},
		OnAppend: func(l local.Sealed) {
			if sink != nil {
				sink.Add(ocsf.Line{Event: l.Event, Prev: l.Prev, Hash: l.Hash, Pos: l.End})
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	sink, err = ocsf.OpenSink(ocsf.SinkOptions{Path: path, Map: ocsf.Options{Hostname: "h", Tenant: rec.Tenant()},
		Read: func(id string, from ocsf.Cursor) ([]ocsf.Line, error) {
			readsMu.Lock()
			reads = append(reads, from)
			readsMu.Unlock()
			sealed, _, err := rec.LinesFrom(id, from.Pos)
			var out []ocsf.Line
			for _, s := range sealed {
				if s.Seq > from.Seq {
					out = append(out, ocsf.Line{Event: s.Event, Prev: s.Prev, Hash: s.Hash, Pos: s.End})
				}
			}
			return out, err
		},
		Sessions: func() (map[string]ocsf.Cursor, error) {
			ids, err := rec.SessionIDs()
			out := map[string]ocsf.Cursor{}
			for _, id := range ids {
				if size, err := rec.Size(id); err == nil && size > 0 {
					out[id] = ocsf.Cursor{Pos: size}
				}
			}
			return out, err
		}})
	if err != nil {
		t.Fatal(err)
	}
	return rec, sink
}

func newSession(t *testing.T, rec *local.Store, id string) *agent.Recorder {
	t.Helper()
	if err := rec.CreateSession(context.Background(), store.SessionRecord{ID: id, User: "tester", Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	return agent.NewRecorder(rec, id, "")
}

func readEvents(t *testing.T, path string) []ocsf.Line {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []ocsf.Line
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		l, err := ocsf.ReplayJSON(sc.Bytes())
		if err != nil {
			t.Fatalf("%v: %s", err, sc.Text())
		}
		out = append(out, l)
	}
	return out
}

// The file is derived from the chain: every line comes back from it with a
// hash the record's own check accepts, a stored secret redacted as in the
// record, and the cursor carries over a restart without a repeat or a gap.
func TestSinkIsTheRecordAndSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "ocsf", "events.jsonl")

	rec, sink := recordWithSink(t, dir, path)
	r := newSession(t, rec, "s-1")
	for _, text := range []string{"first", "use sk-CANARY-4242 here", "third"} {
		if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	// Written while no sink runs: caught up when the next one starts.
	if _, err := r.Record(agent.EvAgentMessage, agent.ActorAgent, agent.Trusted, agent.Message{Text: "while off"}); err != nil {
		t.Fatal(err)
	}
	_ = rec.Close()

	rec, sink = recordWithSink(t, dir, path)
	r = agent.NewRecorder(rec, "s-1", "")
	r.Advance(4)
	if _, err := r.Record(agent.EvAgentMessage, agent.ActorAgent, agent.Trusted, agent.Message{Text: "after"}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("sk-CANARY-4242")) {
		t.Fatal("the OCSF file holds a secret the record redacted")
	}
	if !bytes.Contains(data, []byte("[REDACTED:api_key]")) {
		t.Fatal("the redacted message is not in the file")
	}
	got := readEvents(t, path)
	sealed, err := rec.SealedSince("s-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(sealed) {
		t.Fatalf("the file has %d events, the record %d lines", len(got), len(sealed))
	}
	prev := local.Genesis
	for i, l := range got {
		if l.Seq != sealed[i].Seq || l.ID != sealed[i].ID {
			t.Fatalf("event %d is seq %d, the record's line seq %d", i, l.Seq, sealed[i].Seq)
		}
		if l.Prev != prev {
			t.Fatalf("seq %d: prev %s, want %s", l.Seq, l.Prev, prev)
		}
		h, err := local.LineHash(l.Event, l.Prev)
		if err != nil || h != l.Hash || h != sealed[i].Hash {
			t.Fatalf("seq %d: the replayed line hashes to %s, the record says %s (%v)", l.Seq, h, sealed[i].Hash, err)
		}
		prev = l.Hash
	}
}

// Turning the sink on does not write the history before it: that is what
// `abhed logs` is for.
func TestSinkStartsAtThePresentEnd(t *testing.T) {
	dir := t.TempDir()
	rec, err := local.Open(local.Options{Dir: dir, User: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	r := newSession(t, rec, "s-old")
	if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "old"}); err != nil {
		t.Fatal(err)
	}
	_ = rec.Close()

	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, sink := recordWithSink(t, dir, path)
	r = newSession(t, rec, "s-new")
	if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "new"}); err != nil {
		t.Fatal(err)
	}
	_ = sink.Close()
	_ = rec.Close()
	for _, l := range readEvents(t, path) {
		if l.SessionID == "s-old" {
			t.Fatal("the sink wrote a session's history from before it was turned on")
		}
	}
	if n := len(readEvents(t, path)); n == 0 {
		t.Fatal("the new session was not written")
	}
}

func TestSinkRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, err := local.Open(local.Options{Dir: dir, User: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	sink, err := ocsf.OpenSink(ocsf.SinkOptions{Path: path, MaxBytes: 2048, Keep: 2, Version: "1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	r := newSession(t, rec, "s-1")
	for i := 0; i < 40; i++ {
		ev, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: strings.Repeat("x", 100)})
		if err != nil {
			t.Fatal(err)
		}
		sink.Add(ocsf.Line{Event: ev})
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + ".1", path + ".2"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s is %v", p, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatal("more rotated files kept than asked")
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte(`"version":"1.1.0"`)) {
		t.Fatal("the sink did not write the version asked for")
	}
}

// Lines the queue had no room for are read back from the record: the file
// still holds each line once, in the record's order.
func TestSinkCatchesUpWhatTheQueueDropped(t *testing.T) {
	defer ocsf.SetSinkQueue(1)()
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, sink := recordWithSink(t, dir, path)
	r := newSession(t, rec, "s-1")
	for i := 0; i < 300; i++ {
		if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	got := readEvents(t, path)
	sealed, _ := rec.SealedSince("s-1", 0)
	if len(got) != len(sealed) {
		t.Fatalf("the file has %d events, the record %d lines", len(got), len(sealed))
	}
	for i := range got {
		if got[i].Seq != sealed[i].Seq {
			t.Fatalf("event %d is seq %d, want %d", i, got[i].Seq, sealed[i].Seq)
		}
	}
}

// Lines a writer that crashed left past the head it last synced are found
// from the file itself, not the head, and written when a sink next starts.
func TestSinkCatchesUpPastAStaleHead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, sink := recordWithSink(t, dir, path)
	r := newSession(t, rec, "s-1")
	if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "one"}); err != nil {
		t.Fatal(err)
	}
	_ = sink.Close()
	_ = rec.Close()

	// A writer that never closes, as one that crashed: its last lines are
	// past the head it synced at the user's message.
	crashed, err := local.Open(local.Options{Dir: dir, User: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = crashed.Close() })
	w := agent.NewRecorder(crashed, "s-1", "")
	w.Advance(1)
	for i := 0; i < 3; i++ {
		if _, err := w.Record(agent.EvAgentMessage, agent.ActorAgent, agent.Trusted, agent.Message{Text: "two"}); err != nil {
			t.Fatal(err)
		}
	}
	readsMu.Lock()
	reads = nil
	readsMu.Unlock()
	rec, sink = recordWithSink(t, dir, path)
	defer rec.Close()
	if seq, _, _ := rec.Index().Head("s-1"); seq != 1 {
		t.Fatalf("the head is at seq %d; the test needs one left behind at 1", seq)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readEvents(t, path); len(got) != 4 || got[3].Seq != 4 {
		t.Fatalf("the file holds %d events", len(got))
	}
	// The catch-up read on from the cursor's place in the file, not its start.
	readsMu.Lock()
	defer readsMu.Unlock()
	for _, c := range reads {
		if c.Pos == 0 {
			t.Fatalf("a catch-up read the session from its start: %+v", reads)
		}
	}
	if len(reads) == 0 {
		t.Fatal("nothing was read back from the record")
	}
}

func readEventsIfAny(t *testing.T, path string) []ocsf.Line {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return readEvents(t, path)
}

// A file that cannot be written is retried after a growing wait, not at
// once: here its path is a folder, and the record is read a handful of
// times in half a second, not thousands.
func TestSinkBacksOffAFailedWrite(t *testing.T) {
	path := t.TempDir() // a folder: every append to it fails
	var reads atomic.Int32
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	line := ocsf.Line{Event: agent.Event{ID: "e-1", SessionID: "s-1", Seq: 1, Type: agent.EvUserMessage,
		Payload: []byte(`{"text":"m"}`), Actor: agent.ActorUser, Trust: agent.Trusted, CreatedAt: at}}
	sink, err := ocsf.OpenSink(ocsf.SinkOptions{Path: path, Cursors: &memCursors{},
		Read: func(string, ocsf.Cursor) ([]ocsf.Line, error) {
			reads.Add(1)
			return []ocsf.Line{line}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	sink.Add(line)
	time.Sleep(500 * time.Millisecond)
	n := reads.Load()
	_ = sink.Close()
	if n == 0 || n > 6 {
		t.Fatalf("the record was read %d times in 500 ms after a failed write; want a few, backing off", n)
	}
	if sink.Err() == nil {
		t.Fatal("the failed write was not reported")
	}
}

// memCursors keeps cursors in memory, for a sink whose file is the test.
type memCursors struct {
	mu   sync.Mutex
	rows map[string]ocsf.Cursor
}

func (m *memCursors) Lock() (func(), error) { m.mu.Lock(); return m.mu.Unlock, nil }
func (m *memCursors) Load(_ []string) (map[string]ocsf.Cursor, bool, error) {
	out := map[string]ocsf.Cursor{}
	for k, v := range m.rows {
		out[k] = v
	}
	return out, m.rows != nil, nil
}
func (m *memCursors) Save(c map[string]ocsf.Cursor) error {
	if m.rows == nil {
		m.rows = map[string]ocsf.Cursor{}
	}
	for k, v := range c {
		m.rows[k] = v
	}
	return nil
}

// The cursor's provider is the one the lines up to it said, so a sink takes
// it over what it remembered: another may have written lines since.
func TestSinkPrefersTheCursorsProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	cur := &memCursors{}
	sink, err := ocsf.OpenSink(ocsf.SinkOptions{Path: path, Cursors: cur})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	line := func(seq int64, typ agent.EventType, payload string) ocsf.Line {
		return ocsf.Line{Event: agent.Event{ID: fmt.Sprintf("e-%d", seq), SessionID: "s-1", Seq: seq, Type: typ,
			Payload: []byte(payload), Actor: agent.ActorSystem, Trust: agent.Trusted, CreatedAt: at}}
	}
	sink.Add(line(1, agent.EvSessionStarted, `{"provider":"first","model":"m"}`))
	deadline := time.Now().Add(5 * time.Second)
	for len(readEventsIfAny(t, path)) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Another writer took seq 2, a model.switched, and moved the cursor on.
	cur.mu.Lock()
	cur.rows["s-1"] = ocsf.Cursor{Seq: 2, Provider: "second", Primed: true}
	cur.mu.Unlock()
	sink.Add(line(3, agent.EvModelCall, `{"model":"m","tokens_in":1,"tokens_out":1}`))
	_ = sink.Close()
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"ai_provider":"second"`) {
		t.Fatalf("the model call did not take the cursor's provider: %s", data)
	}
}

// A session that keeps failing grows the backoff, whatever another session
// written meanwhile does: here every read of its cursor fails, and the
// other session's lines are written all along.
func TestBackoffGrowsDespiteOtherSessionsWritten(t *testing.T) {
	cur := &badCursors{}
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	sink, err := ocsf.OpenSink(ocsf.SinkOptions{Path: filepath.Join(t.TempDir(), "b.jsonl"), Cursors: cur,
		Read: func(string, ocsf.Cursor) ([]ocsf.Line, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	add := func(session string, seq int64) {
		sink.Add(ocsf.Line{Event: agent.Event{ID: fmt.Sprintf("%s-%d", session, seq), SessionID: session, Seq: seq,
			Type: agent.EvUserMessage, Payload: []byte(`{"text":"m"}`), Actor: agent.ActorUser, Trust: agent.Trusted, CreatedAt: at}})
	}
	add("bad", 1)
	// Its first failure before the other session's lines, so no batch holds both.
	for cur.badTries.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 80; i++ {
		add("good", int64(i+1))
		time.Sleep(10 * time.Millisecond)
	}
	_ = sink.Close()
	// 800 ms: a backoff of 100, 200 and 400 ms tries it about four times; one
	// reset by every good write would try it every 100 ms or more often.
	if n := cur.badTries.Load(); n == 0 || n > 5 {
		t.Fatalf("the failing session was tried %d times in 800 ms; want a few, backing off", n)
	}
}

// badCursors fails every load that names the session "bad".
type badCursors struct {
	memCursors
	badTries atomic.Int32
}

func (b *badCursors) Load(ids []string) (map[string]ocsf.Cursor, bool, error) {
	for _, id := range ids {
		if id == "bad" {
			b.badTries.Add(1)
			return nil, false, errors.New("the cursor of bad cannot be read")
		}
	}
	return b.memCursors.Load(ids)
}

// scriptedCursors are memCursors whose Lock answers ErrStandby the next
// standby times, and whose Load fails for a session the next failLoad times.
type scriptedCursors struct {
	memCursors
	standby  atomic.Int32
	failFor  string
	failLoad atomic.Int32
}

func (c *scriptedCursors) Lock() (func(), error) {
	if c.standby.Load() > 0 {
		c.standby.Add(-1)
		return nil, ocsf.ErrStandby
	}
	return c.memCursors.Lock()
}

func (c *scriptedCursors) Load(ids []string) (map[string]ocsf.Cursor, bool, error) {
	for _, id := range ids {
		if id == c.failFor && c.failLoad.Load() > 0 {
			c.failLoad.Add(-1)
			return nil, false, errors.New("the cursor cannot be read")
		}
	}
	return c.memCursors.Load(ids)
}

// A sink refused the lock while writing stands by: when it next has the
// lock its scan looks at every session, not only those written lately, and
// what it writes takes the provider from the cursor.
func TestStandbyInWriteMeansAFullScanNext(t *testing.T) {
	st := agent.NewMemStore()
	cur := &scriptedCursors{}
	cur.rows = map[string]ocsf.Cursor{}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	ev := func(session string, seq int64, typ agent.EventType, payload string) agent.Event {
		return agent.Event{ID: fmt.Sprintf("%s-%d", session, seq), SessionID: session, Seq: seq, Type: typ,
			Payload: []byte(payload), Actor: agent.ActorSystem, Trust: agent.Trusted, CreatedAt: at}
	}
	_ = st.Append(ev("s-2", 1, agent.EvSessionStarted, `{"provider":"first","model":"m"}`))
	// Another writer wrote s-2's first line, and the provider it then had.
	cur.rows["s-2"] = ocsf.Cursor{Seq: 1, Provider: "second", Primed: true}
	sink, err := ocsf.OpenSink(ocsf.SinkOptions{Path: path, Cursors: cur, Poll: 300 * time.Millisecond,
		Read: func(id string, from ocsf.Cursor) ([]ocsf.Line, error) {
			evs, err := st.Since(id, from.Seq)
			out := make([]ocsf.Line, len(evs))
			for i, e := range evs {
				out[i] = ocsf.Line{Event: e}
			}
			return out, err
		},
		Sessions: func() (map[string]ocsf.Cursor, error) {
			out := map[string]ocsf.Cursor{}
			for id, seq := range st.Ends() {
				out[id] = ocsf.Cursor{Seq: seq}
			}
			return out, nil
		},
		// A poll by a writer that never stood by asks only for what changed lately: nothing here.
		Changed: func() (map[string]ocsf.Cursor, error) { return map[string]ocsf.Cursor{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	// s-2 grows where no recent list shows it.
	_ = st.Append(ev("s-2", 2, agent.EvModelCall, `{"model":"m","tokens_in":1,"tokens_out":1}`))
	// The next lock, taken by this write, is refused.
	cur.standby.Store(1)
	sink.Add(ocsf.Line{Event: ev("s-1", 1, agent.EvUserMessage, `{"text":"m"}`)})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), `"correlation_uid":"s-2"`) {
			if !strings.Contains(string(data), `"ai_provider":"second"`) {
				t.Fatalf("s-2's model call did not take the cursor's provider: %s", data)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("after standing by, the sink never scanned every session and s-2 was not written")
}

// A session the record no longer holds reads as no lines: it stops counting
// as failing, leaves no backoff behind, and the feed of the others goes on.
func TestAGoneSessionDoesNotHoldTheFile(t *testing.T) {
	cur := &scriptedCursors{failFor: "gone"}
	cur.failLoad.Store(1)
	path := filepath.Join(t.TempDir(), "g.jsonl")
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var seq atomic.Int64
	sink, err := ocsf.OpenSink(ocsf.SinkOptions{Path: path, Cursors: cur, Poll: 10 * time.Millisecond,
		Read: func(id string, _ ocsf.Cursor) ([]ocsf.Line, error) {
			// A deleted session reads as no lines, as both stores give it.
			return nil, nil
		},
		Feed: func() ([]ocsf.Line, func() error, error) {
			n := seq.Add(1)
			return []ocsf.Line{{Event: agent.Event{ID: fmt.Sprintf("ok-%d", n), SessionID: "ok", Seq: n, Type: agent.EvUserMessage,
				Payload: []byte(`{"text":"m"}`), Actor: agent.ActorUser, Trust: agent.Trusted, CreatedAt: at}}}, nil, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	// One failed write of "gone" starts a backoff.
	sink.Add(ocsf.Line{Event: agent.Event{ID: "g-1", SessionID: "gone", Seq: 1, Type: agent.EvUserMessage,
		Payload: []byte(`{"text":"m"}`), Actor: agent.ActorUser, Trust: agent.Trusted, CreatedAt: at}})
	time.Sleep(time.Second)
	_ = sink.Close()
	// Polls every 10 ms for a second; a backoff held by "gone" (100, 200,
	// 400 ms and on) would leave a handful.
	if n := len(readEventsIfAny(t, path)); n < 40 {
		t.Fatalf("the feed wrote %d events in a second; the gone session held the file", n)
	}
}

// A session that failed, then caught up with nothing to write, no longer
// counts as failing: the next failure starts again at the first wait.
func TestCaughtUpWithNothingEndsTheBackoff(t *testing.T) {
	cur := &scriptedCursors{failFor: "s"}
	cur.rows = map[string]ocsf.Cursor{}
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	sink, err := ocsf.OpenSink(ocsf.SinkOptions{Path: filepath.Join(t.TempDir(), "c.jsonl"), Cursors: cur,
		Read: func(string, ocsf.Cursor) ([]ocsf.Line, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	fail := func(seq int64) {
		cur.failLoad.Store(1)
		sink.Add(ocsf.Line{Event: agent.Event{ID: fmt.Sprintf("s-%d", seq), SessionID: "s", Seq: seq, Type: agent.EvUserMessage,
			Payload: []byte(`{"text":"m"}`), Actor: agent.ActorUser, Trust: agent.Trusted, CreatedAt: at}})
		deadline := time.Now().Add(5 * time.Second)
		for cur.failLoad.Load() != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	fail(1)
	time.Sleep(20 * time.Millisecond)
	if got := sink.Backoff(); got != ocsf.MinBackoff() {
		t.Fatalf("the first failure waits %v, want %v", got, ocsf.MinBackoff())
	}
	// The backoff ends, the catch-up finds nothing to write, and the session
	// is caught up.
	time.Sleep(ocsf.MinBackoff() + 100*time.Millisecond)
	fail(2)
	time.Sleep(20 * time.Millisecond)
	if got := sink.Backoff(); got != ocsf.MinBackoff() {
		t.Fatalf("a failure after catching up waits %v, want %v again: the session still counted as failing", got, ocsf.MinBackoff())
	}
}
