package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// heldWriter is an SSE client that stops reading while hold is open, so the
// store's buffer for it fills and later events are dropped.
type heldWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	header http.Header
	hold   chan struct{}
	held   chan struct{}
	once   sync.Once
}

func (h *heldWriter) Header() http.Header { return h.header }
func (h *heldWriter) WriteHeader(int)     {}
func (h *heldWriter) Flush()              {}
func (h *heldWriter) Write(p []byte) (int, error) {
	h.mu.Lock()
	hold := h.hold
	h.mu.Unlock()
	if hold != nil {
		h.once.Do(func() { close(h.held) })
		<-hold
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.buf.Write(p)
}

func (h *heldWriter) ids() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []int64
	for _, m := range regexp.MustCompile(`(?m)^id: (\d+)$`).FindAllStringSubmatch(h.buf.String(), -1) {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		out = append(out, n)
	}
	return out
}

func (h *heldWriter) waitFor(t *testing.T, seq int64) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ids := h.ids(); len(ids) > 0 && ids[len(ids)-1] >= seq {
			return
		}
	}
	t.Fatalf("the stream never reached #%d; got %v", seq, h.ids())
}

// A subscriber that falls behind has events dropped by the store rather than
// stalling the loop. The stream must still deliver every event once, in order.
func TestStreamFillsEventsTheStoreDropped(t *testing.T) {
	q := newQueueRig(t)
	defer close(q.adapter.gate)
	q.s.mu.RLock()
	rec := q.s.running[q.id].Loop.Recorder
	q.s.mu.RUnlock()
	record := func() int64 {
		ev, err := rec.Record(agent.EvAgentDelta, agent.ActorAgent, agent.Trusted, agent.Delta{Text: "x"})
		if err != nil {
			t.Fatal(err)
		}
		return ev.Seq
	}

	evs, _ := q.s.store.Events(q.id)
	start := evs[len(evs)-1].Seq
	hw := &heldWriter{header: http.Header{}, held: make(chan struct{})}
	req := httptest.NewRequest("GET", "/v1/sessions/"+q.id+"/events", nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	req.Header.Set("X-Abhed-User", "alice")
	ctx, cancel := context.WithCancel(req.Context())
	done := make(chan struct{})
	go func() { q.h.ServeHTTP(hw, req.WithContext(ctx)); close(done) }()
	hw.waitFor(t, start)
	time.Sleep(100 * time.Millisecond) // past the backlog, into the subscription

	release := make(chan struct{})
	hw.mu.Lock()
	hw.hold = release
	hw.mu.Unlock()
	record()
	<-hw.held
	for i := 0; i < 400; i++ { // well past the subscriber's buffer of 256
		record()
	}
	hw.mu.Lock()
	hw.hold = nil
	hw.mu.Unlock()
	close(release)
	last := record()
	hw.waitFor(t, last)
	cancel()
	<-done

	ids := hw.ids()
	if int64(len(ids)) != last {
		t.Fatalf("stream carried %d events, want %d", len(ids), last)
	}
	for i, id := range ids {
		if id != int64(i+1) {
			t.Fatalf("event %d on the stream is #%d; want every seq once, in order", i, id)
		}
	}
}

func settleRig(t *testing.T) (*liveSession, *agent.MemStore) {
	t.Helper()
	sess, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := agent.NewMemStore()
	loop := agent.NewLoop(stubAdapter{}, tools.NewRegistry(tools.Read{}), policy.New(policy.ModeAuto),
		agent.AutoApprove{Yes: true}, sess, agent.NewRecorder(store, "s", ""), agent.DefaultConfig())
	return &liveSession{Loop: loop, State: "running", ran: make(chan struct{})}, store
}

func closed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// settle is the handoff between a run ending and a message queued too late
// for its last look: only a clean end with something waiting runs again.
func TestSettle(t *testing.T) {
	live, cancelled := context.Background(), func() context.Context {
		c, cancel := context.WithCancel(context.Background())
		cancel()
		return c
	}()
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		reason agent.TerminalReason
		err    error
		queued bool
		again  bool
	}{
		{"queued and completed", live, agent.TermCompleted, nil, true, true},
		{"queued but cancelled", cancelled, agent.TermUserInterrupt, nil, true, false},
		{"queued but failed", live, agent.TermError, errors.New("model down"), true, false},
		{"queued but out of turns", live, agent.TermMaxTurns, nil, true, false},
		{"completed, nothing queued", live, agent.TermCompleted, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := settleRig(t)
			ran := l.ran
			if tc.queued {
				l.Loop.Queue("late")
			}
			if got := l.settle(tc.ctx, tc.reason, tc.err); got != tc.again {
				t.Fatalf("settle = %v, want %v", got, tc.again)
			}
			if tc.again {
				if closed(ran) || l.State != "running" {
					t.Fatalf("a run going again must stay running with ran open (state %s)", l.State)
				}
				return
			}
			if !closed(ran) || l.State != "done" || l.ran != nil {
				t.Fatalf("an ended run must be done with ran closed (state %s)", l.State)
			}
			// Closed once: a second settle must not close it again.
			if l.settle(tc.ctx, tc.reason, tc.err) {
				t.Fatal("a settled run asked to run again")
			}
		})
	}
}

// A message withdrawn between settle and RunQueued leaves nothing to run: the
// loop reports completed and records no second session.ended.
func TestSettleThenWithdrawn(t *testing.T) {
	l, store := settleRig(t)
	id := l.Loop.Queue("late")
	if !l.settle(context.Background(), agent.TermCompleted, nil) {
		t.Fatal("settle did not run again for a queued message")
	}
	l.mu.Lock()
	l.Loop.Unqueue(id)
	l.mu.Unlock()
	reason, err := l.Loop.RunQueued(context.Background())
	if err != nil || reason != agent.TermCompleted {
		t.Fatalf("RunQueued = %s, %v; want completed", reason, err)
	}
	if l.settle(context.Background(), reason, err) {
		t.Fatal("settle ran again with nothing queued")
	}
	evs, _ := store.Events("s")
	for _, ev := range evs {
		if ev.Type == agent.EvSessionEnded {
			t.Fatal("RunQueued with nothing queued recorded a session.ended")
		}
	}
}

// The session routes that act on a running loop answer for a session on
// another node the way approve does: 421 and the node to go to.
func TestSessionRoutesPointToTheNodeRunningIt(t *testing.T) {
	for _, node := range []string{"node-b", ""} {
		s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
			Registry: tools.NewRegistry(tools.Read{}), NodeID: "node-a",
			Store: &fakeRouter{EventStore: agent.NewMemStore(), node: node}})
		for _, r := range [][2]string{{"GET", "/queue"}, {"DELETE", "/queue/q_1"}, {"POST", "/interrupt"}} {
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(r[0], "/v1/sessions/s-elsewhere"+r[1], nil))
			want, header := http.StatusMisdirectedRequest, "node-b"
			if node == "" {
				want, header = http.StatusNotFound, ""
			}
			if rec.Code != want || rec.Header().Get("Abhed-Session-Node") != header {
				t.Errorf("%s %s with the session on %q = %d node %q, want %d %q",
					r[0], r[1], node, rec.Code, rec.Header().Get("Abhed-Session-Node"), want, header)
			}
		}
	}
}

// A client's id for its message comes back on the user.message that records
// it, queued or not, so the client need not match by text.
func TestClientIDIsEchoed(t *testing.T) {
	q := newQueueRig(t)
	rec := q.do("alice", "POST", "/messages", `{"prompt":"with an id","client_id":"c-2"}`)
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusAccepted || out["queue_id"] == "" {
		t.Fatalf("queued send = %d %s", rec.Code, rec.Body)
	}
	if rec := q.do("alice", "POST", "/messages", `{"prompt":"x","client_id":"<bad id>"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a malformed client_id was accepted: %d", rec.Code)
	}
	close(q.adapter.gate)
	q.waitEnded()
	got := q.userMessages()
	if len(got) != 2 || got[1].ClientID != "c-2" || got[1].QueueID != out["queue_id"] {
		t.Fatalf("user messages = %+v, want the queued one with client_id c-2", got)
	}

	s := testServer(t)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"hi","client_id":"c-1"}`)))
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		evs, _ := s.store.Events(created.SessionID)
		for _, ev := range evs {
			if ev.Type == agent.EvUserMessage {
				var m agent.Message
				_ = json.Unmarshal(ev.Payload, &m)
				if m.ClientID != "c-1" {
					t.Fatalf("first user.message = %+v, want client_id c-1", m)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no user.message recorded")
		}
	}
}
