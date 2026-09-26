package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// shutdownServer starts a session whose turn is busy, and a listener that
// the returned stop shuts down; stop returns once ListenAndServe has.
func shutdownServer(t *testing.T, mk func(ws string) model.Adapter, slowRow bool) (*Server, string, func()) {
	t.Helper()
	cfg := config.Default()
	cfg.Permissions.Mode = "default"
	ws := tempDirResolved(t)
	var st EventStore
	if slowRow {
		// A row write that does not return, whatever its context says.
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		rows := newApprovalRows(agent.NewMemStore())
		rows.askHook = func(context.Context) error { <-release; return nil }
		st = rows
	}
	s := New(Options{Workspace: ws, Config: cfg, Adapter: mk(ws), Addr: "127.0.0.1:0", Store: st,
		Registry: tools.NewRegistry(tools.Read{}, tools.Write{}), Logger: discardLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.ListenAndServe(ctx) }()
	id, err := s.StartSession(context.Background(), StartSpec{Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	return s, id, func() {
		cancel()
		select {
		case <-served:
		case <-time.After(30 * time.Second):
			t.Fatal("ListenAndServe did not return")
		}
	}
}

func tempDirResolved(t *testing.T) string {
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return dir
}

func endedAs(t *testing.T, s *Server, id string) string {
	t.Helper()
	evs, err := s.store.Events(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Type == agent.EvSessionEnded {
			var p agent.SessionEnded
			_ = json.Unmarshal(e.Payload, &p)
			return string(p.Reason)
		}
	}
	return ""
}

func waitState(t *testing.T, s *Server, id, state string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.RLock()
		live := s.running[id]
		s.mu.RUnlock()
		live.mu.Lock()
		got := live.State
		live.mu.Unlock()
		if got == state {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session never reached %s", state)
}

// A shutdown while a turn waits for approval records the turn's end, as a
// shutdown, before ListenAndServe returns and the process can exit; also when
// the approval's row is still being written to a store that does not answer.
func TestShutdownRecordsTheEndOfATurnWaitingForApproval(t *testing.T) {
	for _, slow := range []bool{false, true} {
		s, id, stop := shutdownServer(t, func(ws string) model.Adapter {
			return &editingAdapter{calls: [][]model.ToolCall{{
				toolCall("c1", "write", map[string]string{"path": filepath.Join(ws, "x.txt"), "content": "x"}),
			}}}
		}, slow)
		waitState(t, s, id, "waiting_approval")
		start := time.Now()
		stop()
		if got := endedAs(t, s, id); got != string(agent.TermShutdown) {
			t.Fatalf("slow row=%v: session.ended reason %q once shut down, want %q", slow, got, agent.TermShutdown)
		}
		if took := time.Since(start); took > turnEndWait+time.Second {
			t.Fatalf("slow row=%v: shutdown took %v", slow, took)
		}
	}
}

// The same for a turn in the middle of a model call.
func TestShutdownRecordsTheEndOfAStreamingTurn(t *testing.T) {
	ad := newGateAdapter()
	s, id, stop := shutdownServer(t, func(string) model.Adapter { return ad }, false)
	<-ad.entered
	stop()
	if got := endedAs(t, s, id); got != string(agent.TermShutdown) {
		t.Fatalf("session.ended reason %q once shut down, want %q", got, agent.TermShutdown)
	}
}

// listening starts a server with no session and returns its stop.
func listening(t *testing.T, ad model.Adapter, ws string) (*Server, func()) {
	t.Helper()
	cfg := config.Default()
	cfg.Permissions.Mode = "default"
	s := New(Options{Workspace: ws, Config: cfg, Adapter: ad, Addr: "127.0.0.1:0",
		Registry: tools.NewRegistry(tools.Read{}, tools.Write{}), Logger: discardLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.ListenAndServe(ctx) }()
	return s, func() {
		cancel()
		select {
		case <-served:
		case <-time.After(30 * time.Second):
			t.Fatal("ListenAndServe did not return")
		}
	}
}

func message(s *Server, id, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions/"+id+"/messages", strings.NewReader(body)))
	return rec
}

func endings(t *testing.T, s *Server, id string) []string {
	t.Helper()
	evs, err := s.store.Events(id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		if e.Type == agent.EvSessionEnded {
			var p agent.SessionEnded
			_ = json.Unmarshal(e.Payload, &p)
			out = append(out, string(p.Reason))
		}
	}
	return out
}

// A turn started by a message, as every workbench turn is, is ended on
// shutdown and records it: a workbench's first turn, and a follow-up turn.
func TestShutdownEndsATurnStartedByAMessage(t *testing.T) {
	for _, workbench := range []bool{true, false} {
		ws := tempDirResolved(t)
		write := []model.ToolCall{toolCall("c1", "write", map[string]string{"path": filepath.Join(ws, "x.txt"), "content": "x"})}
		ad := &editingAdapter{calls: [][]model.ToolCall{write}}
		if !workbench {
			// The first turn reads, then answers "done"; the follow-up writes.
			ad.calls = [][]model.ToolCall{{toolCall("c0", "read", map[string]string{"path": ws})}}
		}
		s, stop := listening(t, ad, ws)
		var id string
		var err error
		if workbench {
			id, err = s.openWorkbench(context.Background(), StartSpec{Tenant: "default"})
		} else {
			id, err = s.StartSession(context.Background(), StartSpec{Prompt: "hi", Tenant: "default"})
		}
		if err != nil {
			t.Fatal(err)
		}
		if !workbench {
			waitState(t, s, id, "done")
			ad.mu.Lock()
			ad.calls = append(ad.calls, nil, write)
			ad.mu.Unlock()
		}
		if rec := message(s, id, `{"prompt":"go"}`); rec.Code != http.StatusAccepted {
			t.Fatalf("workbench=%v: message = %d %s", workbench, rec.Code, rec.Body)
		}
		waitState(t, s, id, "waiting_approval")
		start := time.Now()
		stop()
		ends := endings(t, s, id)
		if len(ends) == 0 || ends[len(ends)-1] != string(agent.TermShutdown) {
			t.Fatalf("workbench=%v: endings %q, want the last to be %q", workbench, ends, agent.TermShutdown)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("workbench=%v: shutdown took %v; the turn was waited on, not ended", workbench, took)
		}
	}
}

// While the server is stopping, a message that would start a turn, and a
// Send now, is refused with 503 and told when to retry.
func TestNoTurnStartsWhileDraining(t *testing.T) {
	ws := tempDirResolved(t)
	ad := newGateAdapter()
	s, stop := listening(t, ad, ws)
	defer stop()
	idle, err := s.openWorkbench(context.Background(), StartSpec{Tenant: "default"})
	if err != nil {
		t.Fatal(err)
	}
	busy, err := s.StartSession(context.Background(), StartSpec{Prompt: "hold", Tenant: "default"})
	if err != nil {
		t.Fatal(err)
	}
	<-ad.entered
	s.draining.Store(true)
	for _, c := range []struct{ id, body string }{
		{idle, `{"prompt":"go"}`},
		{busy, `{"prompt":"steer"}`},
		{busy, `{"prompt":"now","interrupt":true}`},
	} {
		rec := message(s, c.id, c.body)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("%s while draining = %d (Retry-After %q), want 503 with Retry-After", c.body, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	// The turn the drain is letting finish is untouched by the refused Send now.
	time.Sleep(50 * time.Millisecond)
	if ends := endings(t, s, busy); len(ends) != 0 {
		t.Fatalf("the running turn ended as %q while draining", ends)
	}
	s.mu.RLock()
	live := s.running[busy]
	s.mu.RUnlock()
	live.mu.Lock()
	state := live.State
	live.mu.Unlock()
	if state != "running" {
		t.Fatalf("the running turn is %q after a refused Send now, want running", state)
	}
	if _, err := s.StartSession(context.Background(), StartSpec{Prompt: "new", Tenant: "default"}); !errors.Is(err, errDraining) {
		t.Fatalf("a new session while draining: %v, want errDraining", err)
	}
}

// drainingRows is a store with session rows whose row write is where the
// drain begins, so a start is refused after its row exists.
type drainingRows struct {
	*agent.MemStore
	s                *Server
	created, deleted []string
}

func (d *drainingRows) CreateSession(_ context.Context, rec store.SessionRecord) error {
	d.created = append(d.created, rec.ID)
	d.s.draining.Store(true)
	return nil
}

func (d *drainingRows) ListSessions(context.Context, int) ([]store.SessionRecord, error) {
	return nil, nil
}

func (d *drainingRows) DeleteSession(id string) error {
	d.deleted = append(d.deleted, id)
	return d.MemStore.DeleteSession(id)
}

// A session refused because the drain began after its row was written is
// removed again, so none is left listed that never ran and never ends.
func TestARefusedStartLeavesNoSession(t *testing.T) {
	for _, workbench := range []bool{false, true} {
		d := &drainingRows{MemStore: agent.NewMemStore()}
		s := New(Options{Workspace: tempDirResolved(t), Config: config.Default(), Adapter: stubAdapter{},
			Registry: tools.NewRegistry(tools.Read{}), Store: d, Logger: discardLogger()})
		d.s = s
		var err error
		if workbench {
			_, err = s.openWorkbench(context.Background(), StartSpec{Tenant: "default"})
		} else {
			_, err = s.StartSession(context.Background(), StartSpec{Prompt: "hi", Tenant: "default"})
		}
		if !errors.Is(err, errDraining) {
			t.Fatalf("workbench=%v: %v, want errDraining", workbench, err)
		}
		if len(d.created) != 1 || len(d.deleted) != 1 || d.deleted[0] != d.created[0] {
			t.Fatalf("workbench=%v: rows created %v, deleted %v", workbench, d.created, d.deleted)
		}
		if evs, _ := d.Events(d.created[0]); len(evs) != 0 {
			t.Fatalf("workbench=%v: %d events left for a refused session", workbench, len(evs))
		}
	}
}
