package server

import (
	"context"
	"encoding/json"
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
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// gateAdapter holds its first model call open until the gate opens or the
// turn is interrupted, so a test can act on a session that is busy.
type gateAdapter struct {
	mu      sync.Mutex
	calls   int
	gate    chan struct{}
	entered chan struct{}
}

func newGateAdapter() *gateAdapter {
	return &gateAdapter{gate: make(chan struct{}), entered: make(chan struct{})}
}

func (a *gateAdapter) Name() string { return "gate" }
func (a *gateAdapter) Profile() model.Profile {
	return model.Profile{Name: "gate", ContextWindow: 32000}
}
func (a *gateAdapter) CountTokens(model.Request) (int, error) { return 10, nil }
func (a *gateAdapter) Complete(ctx context.Context, _ model.Request) (<-chan model.Chunk, error) {
	a.mu.Lock()
	a.calls++
	first := a.calls == 1
	a.mu.Unlock()
	if first {
		close(a.entered)
		select {
		case <-a.gate:
		case <-ctx.Done():
		}
	}
	ch := make(chan model.Chunk, 2)
	ch <- model.Chunk{Type: model.ChunkText, Text: "answer"}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

type queueRig struct {
	t       *testing.T
	s       *Server
	h       http.Handler
	adapter *gateAdapter
	id      string
	ended   chan string
}

// newQueueRig starts a session for alice whose first turn is held open.
func newQueueRig(t *testing.T) *queueRig {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	ad := newGateAdapter()
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: ad,
		Registry: tools.NewRegistry(tools.Read{})})
	rig := &queueRig{t: t, s: s, h: s.Handler(), adapter: ad, ended: make(chan string, 1)}
	id, err := s.StartSession(context.Background(), StartSpec{
		Prompt: "start", Tenant: "acme", User: "alice",
		OnEnd: func(reason string, _ error) { rig.ended <- reason },
	})
	if err != nil {
		t.Fatal(err)
	}
	rig.id = id
	select {
	case <-ad.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first turn never reached the model")
	}
	return rig
}

func (q *queueRig) do(user, method, path, body string) *httptest.ResponseRecorder {
	q.t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/v1/sessions/"+q.id+path, strings.NewReader(body))
	req.Header.Set("X-Abhed-Tenant", "acme")
	req.Header.Set("X-Abhed-User", user)
	q.h.ServeHTTP(rec, req)
	return rec
}

func (q *queueRig) send(prompt string) string {
	q.t.Helper()
	body, _ := json.Marshal(map[string]string{"prompt": prompt})
	rec := q.do("alice", "POST", "/messages", string(body))
	if rec.Code != http.StatusAccepted {
		q.t.Fatalf("send %q = %d %s", prompt, rec.Code, rec.Body)
	}
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["delivery"] != "steered" || out["queue_id"] == "" {
		q.t.Fatalf("a message to a busy session was not queued with an id: %v", out)
	}
	return out["queue_id"]
}

func (q *queueRig) userMessages() []agent.Message {
	q.t.Helper()
	evs, err := q.s.store.Events(q.id)
	if err != nil {
		q.t.Fatal(err)
	}
	var out []agent.Message
	for _, ev := range evs {
		if ev.Type == agent.EvUserMessage {
			var m agent.Message
			_ = json.Unmarshal(ev.Payload, &m)
			out = append(out, m)
		}
	}
	return out
}

func (q *queueRig) waitEnded() string {
	q.t.Helper()
	select {
	case r := <-q.ended:
		return r
	case <-time.After(10 * time.Second):
		q.t.Fatal("the session did not end")
		return ""
	}
}

// A message sent to a busy session is listed with an id, can be withdrawn
// by its owner and nobody else, and the rest arrive in the order sent.
func TestQueueListsCancelsAndDeliversInOrder(t *testing.T) {
	q := newQueueRig(t)
	first := q.send("first")
	second := q.send("second")
	third := q.send("third")

	rec := q.do("alice", "GET", "/queue", "")
	var listed []agent.QueuedMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if rec.Code != http.StatusOK || len(listed) != 3 || listed[0].ID != first || listed[2].ID != third {
		t.Fatalf("GET queue = %d %s", rec.Code, rec.Body)
	}

	if rec := q.do("mallory", "DELETE", "/queue/"+second, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("another user withdrew a queued message: %d", rec.Code)
	}
	if rec := q.do("mallory", "GET", "/queue", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("another user listed the queue: %d", rec.Code)
	}
	if rec := q.do("alice", "DELETE", "/queue/"+second, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE queue = %d %s", rec.Code, rec.Body)
	}
	if rec := q.do("alice", "DELETE", "/queue/"+second, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("a withdrawn message was withdrawn again: %d", rec.Code)
	}

	close(q.adapter.gate)
	if reason := q.waitEnded(); reason != string(agent.TermCompleted) {
		t.Fatalf("session ended %s", reason)
	}
	got := q.userMessages()
	want := []agent.Message{{Text: "start"}, {Text: "first", QueueID: first}, {Text: "third", QueueID: third}}
	if len(got) != len(want) {
		t.Fatalf("user messages = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("user message %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if rec := q.do("alice", "DELETE", "/queue/"+first, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("a delivered message could be withdrawn: %d", rec.Code)
	}
	if rec := q.do("alice", "GET", "/queue", ""); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("queue after delivery = %s, want []", rec.Body)
	}
}

// Send now stops the running turn and starts the message as a fresh one.
func TestInterruptSendsAsAFreshTurn(t *testing.T) {
	q := newQueueRig(t)
	body := `{"prompt":"do this instead","interrupt":true}`
	rec := q.do("alice", "POST", "/messages", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("interrupting send = %d %s", rec.Code, rec.Body)
	}
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["delivery"] != "" {
		t.Fatalf("an interrupting send was queued: %v", out)
	}
	if reason := q.waitEnded(); reason != string(agent.TermUserInterrupt) {
		t.Fatalf("the first run ended %s, want an interrupt", reason)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := q.userMessages()
		if len(got) == 2 && got[1].Text == "do this instead" && got[1].QueueID == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("user messages = %+v, want the new prompt as a fresh turn", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A client opening a new stream resumes after the last event it saw
// rather than receiving the whole session again.
func TestEventsResumeAfterQuery(t *testing.T) {
	s := testServer(t)
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"hello"}`)))
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	var all []agent.Event
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		all, _ = s.store.Events(created.SessionID)
		if len(all) > 0 && all[len(all)-1].Type == agent.EvSessionEnded {
			break
		}
	}
	if len(all) < 3 {
		t.Fatalf("session recorded only %d events", len(all))
	}
	after := all[1].Seq

	stream := func(path string, header string) []int64 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		if header != "" {
			req.Header.Set("Last-Event-ID", header)
		}
		ctx, cancel := context.WithTimeout(req.Context(), 300*time.Millisecond)
		defer cancel()
		h.ServeHTTP(rec, req.WithContext(ctx))
		var ids []int64
		for _, m := range regexp.MustCompile(`(?m)^id: (\d+)$`).FindAllStringSubmatch(rec.Body.String(), -1) {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			ids = append(ids, n)
		}
		return ids
	}
	base := "/v1/sessions/" + created.SessionID + "/events"
	ids := stream(base+"?after="+strconv.FormatInt(after, 10), "")
	if len(ids) != len(all)-2 || ids[0] != after+1 {
		t.Fatalf("?after=%d streamed %v; want every event after it, once", after, ids)
	}
	// The header wins when both are sent, as a browser's own reconnect would.
	if ids := stream(base+"?after=1", strconv.FormatInt(after, 10)); len(ids) == 0 || ids[0] != after+1 {
		t.Fatalf("Last-Event-ID was not preferred over ?after=: %v", ids)
	}
	if ids := stream(base, ""); len(ids) != len(all) {
		t.Fatalf("a fresh stream sent %d of %d events", len(ids), len(all))
	}
}
