package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// hoAdapter asks for a write on "write" and for the blocking tool on "block",
// and answers anything else, a tool's result included, in words.
type hoAdapter struct{ path string }

func (hoAdapter) Name() string                           { return "ho" }
func (hoAdapter) Profile() model.Profile                 { return model.Profile{Name: "ho", ContextWindow: 32000} }
func (hoAdapter) CountTokens(model.Request) (int, error) { return 10, nil }
func (h hoAdapter) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 4)
	last := req.Messages[len(req.Messages)-1]
	switch {
	case last.Role == model.RoleUser && last.Content == "write":
		args, _ := json.Marshal(map[string]string{"path": h.path, "content": "x"})
		c := model.ToolCall{ID: "w1", Name: "write", Args: args}
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &c}
	case last.Role == model.RoleUser && last.Content == "block":
		c := model.ToolCall{ID: "b1", Name: "blocker", Args: json.RawMessage(`{"n":"1"}`)}
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &c}
	case last.Role == model.RoleTool:
		ch <- model.Chunk{Type: model.ChunkText, Text: "done after " + last.ToolCallID}
	default:
		ch <- model.Chunk{Type: model.ChunkText, Text: "ok"}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

// blocker is a read-only tool that runs until released, counting its runs.
type blocker struct {
	runs    atomic.Int32
	started chan struct{}
	release chan struct{}
}

func newBlocker() *blocker {
	return &blocker{started: make(chan struct{}, 4), release: make(chan struct{})}
}
func (*blocker) Name() string        { return "blocker" }
func (*blocker) Description() string { return "waits" }
func (*blocker) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"n":{"type":"string"}}}`)
}
func (*blocker) Mutates() bool { return false }
func (b *blocker) Run(ctx context.Context, _ *tools.Session, _ json.RawMessage) tools.Result {
	b.runs.Add(1)
	b.started <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return tools.Result{Content: "blocked and released"}
}

type hoServer struct {
	t *testing.T
	s *Server
	h http.Handler
}

// newHOServer is one replica; replicas share the workspace, as on one volume.
func newHOServer(t *testing.T, st EventStore, blk *blocker, ws string) *hoServer {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	s := New(Options{Workspace: ws, Config: cfg, Adapter: hoAdapter{path: filepath.Join(ws, "x.txt")}, Store: st, ContinueTurns: true,
		Registry: tools.NewRegistry(tools.Read{}, tools.Write{}, blk)})
	return &hoServer{t: t, s: s, h: s.Handler()}
}

func tempWorkspace(t *testing.T) string {
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	return ws
}

func (h *hoServer) start(prompt string) string {
	h.t.Helper()
	id, err := h.s.StartSession(context.Background(), StartSpec{Prompt: prompt, Tenant: "acme", User: "alice"})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *hoServer) do(method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Abhed-Tenant", "acme")
	req.Header.Set("X-Abhed-User", "alice")
	h.h.ServeHTTP(rec, req)
	return rec
}

func (h *hoServer) live(id string) *liveSession {
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	return h.s.running[id]
}

func (h *hoServer) state(id string) string {
	if l := h.live(id); l != nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.State
	}
	return ""
}

func (h *hoServer) events(id string) []agent.Event {
	evs, _ := h.s.store.Events(id)
	return evs
}

// oneOutcomeEach fails unless every request in the record has exactly one
// approval or denial, recorded before the record's last end.
func oneOutcomeEach(t *testing.T, evs []agent.Event) {
	t.Helper()
	requested := map[string]bool{}
	outcomes := map[string]int{}
	lastEnd := int64(0)
	for _, e := range evs {
		if e.Type == agent.EvSessionEnded {
			lastEnd = e.Seq
		}
	}
	for _, e := range evs {
		var p struct {
			CallID string `json:"call_id"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		switch e.Type {
		case agent.EvActionRequested:
			requested[p.CallID] = true
		case agent.EvActionApproved, agent.EvActionDenied:
			outcomes[p.CallID]++
			if e.Seq > lastEnd {
				t.Errorf("the outcome for %s came after the last end", p.CallID)
			}
		}
	}
	for id := range requested {
		if outcomes[id] != 1 {
			t.Errorf("request %s has %d outcomes", id, outcomes[id])
		}
	}
}

// A replica that drains hands the turn waiting on a question over; another
// replica takes it when a message for it arrives, asks the question again,
// runs the call once it is approved, and goes on with the turn, the message
// steering it. The first replica writes nothing more.
func TestTwoServersOnePostgresDrainHandsTheTurnOver(t *testing.T) {
	blk := newBlocker()
	ws := tempWorkspace(t)
	a := newHOServer(t, openSharedPG(t), blk, ws)
	b := newHOServer(t, openSharedPG(t), blk, ws)
	id := a.start("write")
	waitUntil(t, "A to ask", func() bool { return a.state(id) == "waiting_approval" })
	if _, err := os.Stat(filepath.Join(ws, "x.txt")); err == nil {
		t.Fatal("the write ran before it was approved")
	}
	a.s.drain() // no drain budget: what runs is handed over at once
	if a.live(id) != nil {
		t.Fatal("A kept the session it handed over")
	}
	if h := pgHolder(t, id); h != store.HandedOver {
		t.Fatalf("holder after the hand-over = %q", h)
	}
	if _, ended := agent.LastEnd(b.events(id)); ended {
		t.Fatal("the handed-over run recorded an end")
	}

	if rec := b.do("POST", "/v1/sessions/"+id+"/messages", `{"prompt":"also this"}`); rec.Code != http.StatusAccepted ||
		!strings.Contains(rec.Body.String(), "steered") {
		t.Fatalf("message to the handed-over session = %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "B to ask again", func() bool { return b.state(id) == "waiting_approval" })
	if rec := b.do("POST", "/v1/sessions/"+id+"/approve", `{"approved":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("approve on B = %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "B's turn to end", func() bool {
		end, ok := agent.LastEnd(b.events(id))
		return ok && end.Reason == agent.TermCompleted
	})
	if _, err := os.Stat(filepath.Join(ws, "x.txt")); err != nil {
		t.Fatalf("the write asked again did not run: %v", err)
	}
	evs := b.events(id)
	oneOutcomeEach(t, evs)
	if countType(evs, agent.EvTurnContinued) != 1 || countType(evs, agent.EvSessionEnded) != 1 {
		t.Fatalf("record: %d turn.continued, %d ends", countType(evs, agent.EvTurnContinued), countType(evs, agent.EvSessionEnded))
	}
	var steered bool
	for _, m := range payloadsOf(evs, agent.EvUserMessage) {
		steered = steered || m["text"] == "also this"
	}
	if !steered {
		t.Fatal("the message sent during the hand-over never reached the turn")
	}
}

// A replica that dies while a call runs: once its claim is stale, another
// replica's sweep takes the turn over, answers the call as outcome unknown
// without running it again, and the turn goes on. The dead replica's late
// result is refused.
func TestTwoServersOnePostgresCrashedTurnIsContinued(t *testing.T) {
	blk := newBlocker()
	ws := tempWorkspace(t)
	a := newHOServer(t, openSharedPG(t), blk, ws)
	b := newHOServer(t, openSharedPG(t), blk, ws)
	id := a.start("block")
	<-blk.started
	// A stops beating, as a process that died, and its claim goes stale.
	aLive := a.live(id)
	aLive.mu.Lock()
	stop := aLive.beatStop
	aLive.beatStop = nil
	aLive.mu.Unlock()
	stop()
	pgExec(t, "UPDATE sessions SET node_seen_at = now() - interval '10 minutes' WHERE id = $1", id)

	if n := b.s.RecoverOrphans(context.Background()); n < 1 {
		t.Fatalf("B's sweep took %d sessions over", n)
	}
	waitUntil(t, "B's turn to end", func() bool {
		end, ok := agent.LastEnd(b.events(id))
		return ok && end.Reason == agent.TermCompleted
	})
	if n := blk.runs.Load(); n != 1 {
		t.Fatalf("the call ran %d times", n)
	}
	close(blk.release) // A's call returns now; its result must not land
	waitUntil(t, "A to be fenced", func() bool { return aLive.fenced.Load() })

	evs := b.events(id)
	oneOutcomeEach(t, evs)
	obs := 0
	for _, o := range payloadsOf(evs, agent.EvObservation) {
		if o["call_id"] == "b1" {
			obs++
			if o["outcome_unknown"] != true {
				t.Fatalf("b1 answered without outcome_unknown: %v", o)
			}
		}
	}
	if obs != 1 {
		t.Fatalf("b1 has %d results", obs)
	}
	var said bool
	for _, m := range payloadsOf(evs, agent.EvAgentMessage) {
		said = said || m["text"] == "done after b1"
	}
	if !said {
		t.Fatal("the turn did not go on after the unknown outcome")
	}
	if h := pgHolder(t, id); h == a.s.holder {
		t.Fatal("A holds the session again")
	}
}
