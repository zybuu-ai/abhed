package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// bgAdapter plays a parent that starts one background task per "bg:<name>"
// prompt, and children that wait for their gate.
type bgAdapter struct {
	mu    sync.Mutex
	gates map[string]chan struct{}
	slow  time.Duration
}

func newBGAdapter(names ...string) *bgAdapter {
	a := &bgAdapter{gates: map[string]chan struct{}{}}
	for _, n := range names {
		a.gates[n] = make(chan struct{})
	}
	return a
}

func (a *bgAdapter) release(name string) { close(a.gates[name]) }

func (*bgAdapter) Name() string                           { return "bgs" }
func (*bgAdapter) Profile() model.Profile                 { return model.Profile{Name: "bgs", ContextWindow: 32000} }
func (*bgAdapter) CountTokens(model.Request) (int, error) { return 10, nil }
func (a *bgAdapter) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 4)
	first := req.Messages[0].Content
	last := req.Messages[len(req.Messages)-1]
	a.mu.Lock()
	gate, child := a.gates[first]
	a.mu.Unlock()
	switch {
	case child:
		select {
		case <-gate:
			ch <- model.Chunk{Type: model.ChunkText, Text: "result of " + first}
		case <-ctx.Done():
			ch <- model.Chunk{Type: model.ChunkText, Text: "stopped"}
		}
	case last.Role == model.RoleUser && strings.HasPrefix(last.Content, "bg:"):
		name := strings.TrimPrefix(last.Content, "bg:")
		c := model.ToolCall{ID: "t" + name, Name: "task",
			Args: json.RawMessage(`{"prompt":"` + name + `","description":"` + name + `","background":true}`)}
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &c}
	default:
		if last.Role == model.RoleTool {
			time.Sleep(a.slow) // the run stays live a while after the start
		}
		ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

type bgServer struct {
	t     *testing.T
	s     *Server
	h     http.Handler
	ad    *bgAdapter
	store EventStore
	ended chan string
}

func newBGServer(t *testing.T, st EventStore, names ...string) *bgServer {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	ad := newBGAdapter(names...)
	if st == nil {
		st = agent.NewMemStore()
	}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: ad, Registry: tools.NewRegistry(tools.Read{}), Store: st})
	agent.TurnEndWait = time.Second
	t.Cleanup(func() { agent.TurnEndWait = 5 * time.Second })
	return &bgServer{t: t, s: s, h: s.Handler(), ad: ad, store: st, ended: make(chan string, 4)}
}

func (b *bgServer) start(prompt string, unattended bool) string {
	b.t.Helper()
	id, err := b.s.StartSession(context.Background(), StartSpec{Prompt: prompt, Tenant: "acme", User: "alice",
		Unattended: unattended, OnEnd: func(reason string, _ error) { b.ended <- reason }})
	if err != nil {
		b.t.Fatal(err)
	}
	return id
}

func (b *bgServer) do(user, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Abhed-Tenant", "acme")
	req.Header.Set("X-Abhed-User", user)
	b.h.ServeHTTP(rec, req)
	return rec
}

func (b *bgServer) live(id string) *liveSession {
	b.s.mu.RLock()
	defer b.s.mu.RUnlock()
	return b.s.running[id]
}

func (b *bgServer) state(id string) string {
	l := b.live(id)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.State
}

func (b *bgServer) events(id string) []agent.Event {
	evs, _ := b.store.Events(id)
	return evs
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func countType(evs []agent.Event, typ agent.EventType) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// A child outlives the run that started it: the session is listed
// background with the count, the event stream stays open past the run's
// end, and delivers the result and the closing end; then the session is done.
func TestServerChildOutlivesRunAndStreamStaysOpen(t *testing.T) {
	b := newBGServer(t, nil, "one")
	b.ad.slow = 400 * time.Millisecond
	id := b.start("bg:one", false)

	// The stream is opened while the run is still live, so the run's own end
	// reaches it live.
	srv := httptest.NewServer(b.h)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions/"+id+"/events", nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	req.Header.Set("X-Abhed-User", "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	<-b.ended
	waitUntil(t, "state background", func() bool { return b.state(id) == "background" })
	rec := b.do("alice", "GET", "/v1/sessions", "")
	var list []sessionSummary
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].State != "background" || list[0].Background != 1 {
		t.Fatalf("list: %+v", list)
	}
	time.Sleep(100 * time.Millisecond)
	b.ad.release("one")
	var sawNotice, sawSettled bool
	for line := range lines {
		sawNotice = sawNotice || strings.Contains(line, `"type":"subagent.notice"`)
		sawSettled = sawSettled || strings.Contains(line, `"settled":true`)
	}
	if !sawNotice || !sawSettled {
		t.Fatalf("the stream closed before the result (%v) or the closing end (%v)", sawNotice, sawSettled)
	}
	waitUntil(t, "state done", func() bool { return b.state(id) == "done" })
}

// An approval answered while only children run returns the session to
// background, not running.
func TestApproveRestoresBackgroundState(t *testing.T) {
	b := newBGServer(t, nil, "one")
	id := b.start("bg:one", false)
	<-b.ended
	live := b.live(id)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = live.Approve(ctx, "bash", json.RawMessage(`{"command":"x"}`), policyAsk())
	}()
	waitUntil(t, "the ask", func() bool { return b.state(id) == "waiting_approval" })
	rec := b.do("alice", "GET", "/v1/sessions", "")
	if !strings.Contains(rec.Body.String(), `"pending_ask":{"tool":"bash"`) {
		t.Fatalf("the list does not show the waiting ask: %s", rec.Body)
	}
	cancel()
	<-done
	if st := b.state(id); st != "background" {
		t.Fatalf("after the ask: %s", st)
	}
	b.ad.release("one")
}

// Stop means stop: an interrupt while only children run cancels them all.
func TestInterruptCancelsBackgroundChildren(t *testing.T) {
	b := newBGServer(t, nil, "one")
	id := b.start("bg:one", false)
	<-b.ended
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/interrupt", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("interrupt: %d", rec.Code)
	}
	if n := b.live(id).Loop.Background.Live(); n != 0 {
		t.Fatalf("%d children outlived Stop", n)
	}
	ret := payloadsOf(b.events(id), agent.EvSubagentReturn)
	if len(ret) != 1 || ret[0]["reason"] != string(agent.TermUserInterrupt) {
		t.Fatalf("returned: %v", ret)
	}
	waitUntil(t, "state done", func() bool { return b.state(id) == "done" })
}

func payloadsOf(evs []agent.Event, typ agent.EventType) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if e.Type == typ {
			var m map[string]any
			_ = json.Unmarshal(e.Payload, &m)
			out = append(out, m)
		}
	}
	return out
}

// Deleting a session cancels its children before its rows go, so nothing is
// appended after the delete.
func TestDeleteCancelsBackgroundBeforeRows(t *testing.T) {
	b := newBGServer(t, nil, "one")
	id := b.start("bg:one", false)
	<-b.ended
	child := payloadsOf(b.events(id), agent.EvSubagentSpawned)[0]["session"].(string)
	held := b.live(id)
	if rec := b.do("alice", "DELETE", "/v1/sessions/"+id, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	b.ad.release("one") // a child still running would now finish and write its return
	time.Sleep(300 * time.Millisecond)
	if evs := b.events(id); len(evs) != 0 {
		t.Fatalf("the parent's record got events after its delete: %d", len(evs))
	}
	_ = child
	if tasks := held.Loop.Background.Tasks(); len(tasks) != 1 || tasks[0].Reason != string(agent.TermSessionDeleted) {
		t.Fatalf("the child: %+v", tasks)
	}
	if b.live(id) != nil {
		t.Fatal("the session is still running")
	}
}

// A drain gives children the drain budget, then ends them as shutdown and
// records the closing end, so another node may claim the session.
func TestDrainEndsBackgroundAsShutdown(t *testing.T) {
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}}
	b := newBGServer(t, st, "one")
	b.s.opts.DrainTimeout = 200 * time.Millisecond
	id := b.start("bg:one", false)
	<-b.ended
	if st.ended[id] {
		t.Fatal("the row was released while a child ran")
	}
	if b.s.runningCount() != 1 {
		t.Fatal("a session with children is not counted by the drain")
	}
	b.s.drain()
	ret := payloadsOf(b.events(id), agent.EvSubagentReturn)
	if len(ret) != 1 || ret[0]["reason"] != string(agent.TermShutdown) {
		t.Fatalf("returned: %v", ret)
	}
	end, _ := agent.LastEnd(b.events(id))
	st.mu.Lock()
	released := st.ended[id]
	st.mu.Unlock()
	if !end.Settled || !released {
		t.Fatalf("closing end %+v, row released %v", end, released)
	}
}

// The task list, a task's cancel and the wake switch are the owner's alone.
func TestTaskEndpointsOwnerOnly(t *testing.T) {
	b := newBGServer(t, nil, "one")
	id := b.start("bg:one", false)
	<-b.ended
	base := "/v1/sessions/" + id
	for _, c := range []struct{ method, path, body string }{
		{"GET", base + "/tasks", ""},
		{"POST", base + "/tasks/anything/cancel", ""},
		{"POST", base + "/wake", `{"wake":"off"}`},
	} {
		if rec := b.do("mallory", c.method, c.path, c.body); rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s by another user: %d", c.method, c.path, rec.Code)
		}
	}
	rec := b.do("alice", "GET", base+"/tasks", "")
	var got struct {
		Tasks []agent.TaskInfo `json:"tasks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Tasks) != 1 || got.Tasks[0].Status != "running" {
		t.Fatalf("tasks: %s", rec.Body)
	}
	if rec := b.do("alice", "POST", base+"/wake", `{"wake":"auto"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("wake above the server's ceiling: %d", rec.Code)
	}
	if rec := b.do("alice", "POST", base+"/wake", `{"wake":"off"}`); rec.Code != http.StatusOK {
		t.Fatalf("wake off: %d %s", rec.Code, rec.Body)
	}
	if rec := b.do("alice", "POST", base+"/tasks/"+got.Tasks[0].ID+"/cancel", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", rec.Code)
	}
	if countType(b.events(id), agent.EvWakeSet) != 1 {
		t.Fatal("the wake switch was not recorded")
	}
}

// An unattended run joins its children: its end, and OnEnd, come after theirs.
func TestUnattendedForcedOff(t *testing.T) {
	b := newBGServer(t, nil, "one")
	id := b.start("bg:one", true)
	select {
	case <-b.ended:
		t.Fatal("an unattended session ended with its child still running")
	case <-time.After(200 * time.Millisecond):
	}
	b.ad.release("one")
	select {
	case <-b.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("never ended")
	}
	if end, _ := agent.LastEnd(b.events(id)); end.Background != 0 || countType(b.events(id), agent.EvSubagentNotice) != 1 {
		t.Fatalf("end %+v", end)
	}
}

// routingMem is a store that also routes, and remembers the claims.
type routingMem struct {
	*agent.MemStore
	mu       sync.Mutex
	claims   int
	released bool
}

func (r *routingMem) ClaimNode(context.Context, string, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.claims++
	r.released = false
	return nil
}
func (r *routingMem) ReleaseNode(context.Context, string, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = true
	return nil
}
func (r *routingMem) NodeFor(context.Context, string, time.Duration) (string, error) { return "", nil }
func (r *routingMem) snapshot() (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.claims, r.released
}

// The node keeps its claim fresh while only children run, for a started
// session and for one continued by a message, and lets go once they end.
func TestHeartbeatWhileOnlyChildrenRun(t *testing.T) {
	old := nodeHeartbeat
	nodeHeartbeat = 20 * time.Millisecond
	defer func() { nodeHeartbeat = old }()
	rm := &routingMem{MemStore: agent.NewMemStore()}
	b := newBGServer(t, rm, "one", "two")
	b.s.opts.NodeID = "node-a"
	id := b.start("bg:one", false)
	<-b.ended
	n, _ := rm.snapshot()
	waitUntil(t, "refreshes after the run", func() bool { m, rel := rm.snapshot(); return m >= n+3 && !rel })
	b.ad.release("one")
	waitUntil(t, "the release", func() bool { _, rel := rm.snapshot(); return rel })

	// Continued by a message, which never claimed the node before.
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"bg:two"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("message: %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "state background", func() bool { return b.state(id) == "background" })
	n, _ = rm.snapshot()
	waitUntil(t, "refreshes on a continued session", func() bool { m, rel := rm.snapshot(); return m >= n+3 && !rel })
	b.ad.release("two")
	waitUntil(t, "the release", func() bool { _, rel := rm.snapshot(); return rel })
}

// The row stays open while children run: another node's claim fails.
func TestRowStaysOpenWhileChildrenRun(t *testing.T) {
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}}
	b := newBGServer(t, st, "one")
	id := b.start("bg:one", false)
	<-b.ended
	if ok, _ := st.ClaimResume(context.Background(), id); ok {
		t.Fatal("another node claimed a session whose children run here")
	}
	b.ad.release("one")
	waitUntil(t, "the closing end", func() bool { e, _ := agent.LastEnd(b.events(id)); return e.Settled })
	if ok, _ := st.ClaimResume(context.Background(), id); !ok {
		t.Fatal("the closing end did not release the row")
	}
}

func policyAsk() policy.Result { return policy.Result{Decision: policy.Ask, Reason: "a rule asks"} }

// A result recorded as returned but never delivered, as when a drain ends a
// child, reaches the conversation when another server continues the session.
func TestPendingNoticeRestoredOnServerResume(t *testing.T) {
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}}
	a := newBGServer(t, st, "one")
	a.s.opts.DrainTimeout = 50 * time.Millisecond
	id := a.start("bg:one", false)
	<-a.ended
	a.s.drain()
	if countType(a.events(id), agent.EvSubagentNotice) != 0 || countType(a.events(id), agent.EvSubagentReturn) != 1 {
		t.Fatal("precondition: a return with no notice")
	}
	b := newBGServer(t, st)
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"what happened?"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("continue: %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "the notice", func() bool { return countType(b.events(id), agent.EvSubagentNotice) == 1 })
	n := payloadsOf(b.events(id), agent.EvSubagentNotice)[0]
	if n["delivery"] != "boundary" || n["reason"] != string(agent.TermShutdown) {
		t.Fatalf("notice: %v", n)
	}
}
