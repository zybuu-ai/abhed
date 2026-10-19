package ocsf_test

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/ocsf"
)

// sharedCursors is one set of cursors several servers read, with a lease
// only one holds at a time, as the Postgres writer's lock is.
type sharedCursors struct {
	mu     sync.Mutex
	holder string
	begun  bool
	rows   map[string]ocsf.Cursor
	lock   sync.Mutex
}

type leaseView struct {
	s  *sharedCursors
	me string
}

func (v leaseView) Lock() (func(), error) {
	v.s.mu.Lock()
	if v.s.holder != "" && v.s.holder != v.me {
		v.s.mu.Unlock()
		return nil, ocsf.ErrStandby
	}
	v.s.holder = v.me
	v.s.mu.Unlock()
	v.s.lock.Lock()
	return v.s.lock.Unlock, nil
}

func (v leaseView) Load(_ []string) (map[string]ocsf.Cursor, bool, error) {
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	out := map[string]ocsf.Cursor{}
	for k, c := range v.s.rows {
		out[k] = c
	}
	return out, v.s.begun, nil
}

func (v leaseView) Save(m map[string]ocsf.Cursor) error {
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	v.s.begun = true
	for k, c := range m {
		v.s.rows[k] = c
	}
	return nil
}

func (s *sharedCursors) release(who string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holder == who {
		s.holder = ""
	}
}

// Two servers over one store: the one holding the lease writes, the other
// waits; when the writer goes, the other takes over from the shared cursors.
// Between their files every event is written once.
func TestOneWriterAcrossServers(t *testing.T) {
	st := agent.NewMemStore()
	shared := &sharedCursors{rows: map[string]ocsf.Cursor{}}
	dir := t.TempDir()
	open := func(name string) *ocsf.Sink {
		sink, err := ocsf.OpenSink(ocsf.SinkOptions{
			Path: filepath.Join(dir, name+".jsonl"), Cursors: leaseView{shared, name}, Poll: 10 * time.Millisecond,
			Read: func(id string, from ocsf.Cursor) ([]ocsf.Line, error) {
				evs, err := st.Since(id, from.Seq)
				out := make([]ocsf.Line, len(evs))
				for i, ev := range evs {
					out[i] = ocsf.Line{Event: ev}
				}
				return out, err
			},
			Sessions: func() (map[string]ocsf.Cursor, error) {
				out := map[string]ocsf.Cursor{}
				for id, seq := range st.Ends() {
					out[id] = ocsf.Cursor{Seq: seq}
				}
				return out, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		return sink
	}
	r := agent.NewRecorder(st, "s-1", "")
	say := func(n int) {
		for i := 0; i < n; i++ {
			if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "m"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	a := open("a")
	b := open("b")
	say(5)
	waitFor := func(path string, n int) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if fileEvents(t, path) >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s holds %d events, want %d", path, fileEvents(t, path), n)
	}
	waitFor(filepath.Join(dir, "a.jsonl"), 5)
	// The writer goes; the standby takes the lease at its next poll.
	_ = a.Close()
	shared.release("a")
	say(3)
	waitFor(filepath.Join(dir, "b.jsonl"), 3)
	_ = b.Close()
	if na, nb := fileEvents(t, filepath.Join(dir, "a.jsonl")), fileEvents(t, filepath.Join(dir, "b.jsonl")); na != 5 || nb != 3 {
		t.Fatalf("a wrote %d and b %d; want 5 and 3, each event once", na, nb)
	}
}

func fileEvents(t *testing.T, path string) int {
	t.Helper()
	return len(readEventsIfAny(t, path))
}

// A feed's place moves only once its lines are in the file: a sink that
// does not hold the lease writes nothing and keeps nothing.
func TestFeedMovesOnlyAfterTheWrite(t *testing.T) {
	shared := &sharedCursors{rows: map[string]ocsf.Cursor{}, begun: true, holder: "other"}
	dir := t.TempDir()
	var done atomic.Int32
	at := time.Now()
	feed := func() ([]ocsf.Line, func() error, error) {
		l := ocsf.Line{Event: agent.Event{ID: "e-1", SessionID: "s-1", Seq: 1, Type: agent.EvUserMessage,
			Payload: []byte(`{"text":"m"}`), Actor: agent.ActorUser, Trust: agent.Trusted, CreatedAt: at}}
		return []ocsf.Line{l}, func() error { done.Add(1); return nil }, nil
	}
	path := filepath.Join(dir, "f.jsonl")
	sink, err := ocsf.OpenSink(ocsf.SinkOptions{Path: path, Cursors: leaseView{shared, "me"}, Poll: 10 * time.Millisecond, Feed: feed})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if done.Load() != 0 || fileEvents(t, path) != 0 {
		t.Fatalf("a standby wrote %d and moved its place %d times", fileEvents(t, path), done.Load())
	}
	shared.release("other")
	deadline := time.Now().Add(5 * time.Second)
	for fileEvents(t, path) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = sink.Close()
	if n := fileEvents(t, path); n != 1 || done.Load() == 0 {
		t.Fatalf("wrote %d, moved %d times", n, done.Load())
	}
}
