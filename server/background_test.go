package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
	// askOnWake answers a background result with a command that asks.
	askOnWake bool
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
	case last.Role == model.RoleTool && strings.HasPrefix(last.ToolCallID, "bgn_") && a.askOnWake:
		c := model.ToolCall{ID: "w" + last.ToolCallID, Name: "bash", Args: json.RawMessage(`{"command":"touch woke.txt"}`)}
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
	return newBGServerWith(t, st, nil, names...)
}

func newBGServerWith(t *testing.T, st EventStore, tune func(*config.Config, *Options), names ...string) *bgServer {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	ad := newBGAdapter(names...)
	if st == nil {
		st = agent.NewMemStore()
	}
	opts := Options{Workspace: t.TempDir(), Config: cfg, Adapter: ad, Registry: tools.NewRegistry(tools.Read{}), Store: st}
	if tune != nil {
		tune(&opts.Config, &opts)
	}
	s := New(opts)
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
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed below, after the reader goroutine is done with it
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
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
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
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
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
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
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

// A session a crashed process left open (children running, row never
// ended) is taken over by the next message to it: its lost child is
// reconciled, the result owed arrives, and the conversation goes on.
func TestOrphanReconciledOnClaim(t *testing.T) {
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	a := newBGServer(t, st, "one")
	id := a.start("bg:one", false)
	<-a.ended // a's process now "crashes": its children never end, its row stays open
	if st.ended[id] {
		t.Fatal("precondition: the row is open")
	}
	st.crash(id) // and its heartbeat stops
	b := newBGServer(t, st)
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"still there?"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("continue: %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "the notice", func() bool { return countType(b.events(id), agent.EvSubagentNotice) == 1 })
	var lost, recovered bool
	for _, r := range payloadsOf(b.events(id), agent.EvSubagentReturn) {
		lost = lost || r["reason"] == string(agent.TermLost)
	}
	for _, e := range payloadsOf(b.events(id), agent.EvSessionEnded) {
		recovered = recovered || e["recovered"] == true
	}
	if !lost || !recovered {
		t.Fatalf("lost %v, recovered %v", lost, recovered)
	}
	a.ad.release("one")
}

// A session whose holder is alive is not an orphan, whichever process
// started first and however long since its record was written: a live
// child on one server is never reconciled by another on the same store.
func TestLiveSessionIsNotAnOrphan(t *testing.T) {
	for _, bFirst := range []bool{true, false} {
		st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
		var b *bgServer
		if bFirst {
			b = newBGServer(t, st)
		}
		a := newBGServer(t, st, "one")
		id := a.start("bg:one", false)
		<-a.ended
		if !bFirst {
			time.Sleep(20 * time.Millisecond)
			b = newBGServer(t, st) // started after A's last write, as a restarted peer is
		}
		if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"x"}`); rec.Code != http.StatusConflict {
			t.Fatalf("b first %v: a session with a live holder was taken over: %d", bFirst, rec.Code)
		}
		if n := countType(b.events(id), agent.EvSubagentReturn); n != 0 {
			t.Fatalf("b first %v: the live child was reconciled (%d returns)", bFirst, n)
		}
		if st.holderOf(id) != a.s.holder {
			t.Fatalf("b first %v: holder %q, want A's %q", bFirst, st.holderOf(id), a.s.holder)
		}
		a.ad.release("one")
		waitUntil(t, "the closing end", func() bool { e, _ := agent.LastEnd(a.events(id)); return e.Settled })
	}
}

// Every process has its own liveness identity, and a claim that cannot be
// recorded fails the hold: the session does not run unseen.
func TestHolderIdentityAndFailedHold(t *testing.T) {
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	a, b := newBGServer(t, st), newBGServer(t, st)
	if a.s.holder == "" || a.s.holder == b.s.holder {
		t.Fatalf("holders %q and %q", a.s.holder, b.s.holder)
	}
	st.mu.Lock()
	st.failHold = true
	st.mu.Unlock()
	rec := a.do("alice", "POST", "/v1/sessions", `{"prompt":"hello"}`)
	if rec.Code < 500 {
		t.Fatalf("a session started with no recorded holder: %d %s", rec.Code, rec.Body)
	}
	if a.s.runningCount() != 0 {
		t.Fatal("the refused session is still running")
	}
}

// Continuing a session elsewhere does not reset its allowance.
func TestBudgetCarriedOnResume(t *testing.T) {
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	a := newBGServer(t, st)
	id := a.start("hello", false)
	<-a.ended
	want, _ := agent.CarriedSpend(a.events(id))
	b := newBGServer(t, st)
	live, err := b.s.resumeSession(context.Background(), id, "", "alice", "acme", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := live.Loop.Budget.Spent(); got != want || want == 0 {
		t.Fatalf("carried %d, want %d", got, want)
	}
}

// A resume is only for the owner's own child of the session that started it:
// the store wrapper checks the child's row.
func TestSubSessionOfChecksOwnerAndParent(t *testing.T) {
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	b := newBGServer(t, st)
	st.rows["c1"] = store.SessionRecord{ID: "c1", Tenant: "acme", User: "alice", ParentID: "p1"}
	alice := b.s.subagentStore("p1", StartSpec{Tenant: "acme", User: "alice"}, "default").(agent.SubSessionChecker)
	bob := b.s.subagentStore("p1", StartSpec{Tenant: "acme", User: "bob"}, "default").(agent.SubSessionChecker)
	if ok, _ := alice.SubSessionOf(context.Background(), "c1", "p1"); !ok {
		t.Fatal("the owner's own child was refused")
	}
	for name, c := range map[string]func() (bool, error){
		"another user":   func() (bool, error) { return bob.SubSessionOf(context.Background(), "c1", "p1") },
		"another parent": func() (bool, error) { return alice.SubSessionOf(context.Background(), "c1", "p2") },
		"no such row":    func() (bool, error) { return alice.SubSessionOf(context.Background(), "zz", "p1") },
	} {
		if ok, _ := c(); ok {
			t.Fatalf("%s: allowed", name)
		}
	}
}

// A run that ends while a result is owed but undelivered leaves the session
// in background, not done: the drain waits for it and the claim is kept.
func TestSettleCountsOwedResults(t *testing.T) {
	b := newBGServer(t, nil)
	id := b.start("hello", false)
	<-b.ended
	live := b.live(id)
	live.Loop.QueueNotices([]agent.Notice{{TaskID: "t-1", Session: "t-1", CallID: "bgn_t1", Content: "done"}})
	live.mu.Lock()
	live.ran = make(chan struct{})
	live.mu.Unlock()
	live.settle(context.Background(), agent.TermMaxTurns, nil)
	if got := b.state(id); got != "background" {
		t.Fatalf("state after an end with a result owed: %q, want background", got)
	}
	live.mu.Lock()
	state := live.stateAfterAsk("done")
	live.mu.Unlock()
	if state != "background" {
		t.Fatalf("state after an ask with a result owed: %q", state)
	}
}

// drainedWithResultOwed is a session whose child a drain ended: its return
// is recorded, its notice is not, and its row is released.
func drainedWithResultOwed(t *testing.T) (*durableMem, string) {
	t.Helper()
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	a := newBGServer(t, st, "one")
	a.s.opts.DrainTimeout = 50 * time.Millisecond
	id := a.start("bg:one", false)
	<-a.ended
	a.s.drain()
	if evs, _ := st.Events(id); len(agent.PendingNotices(evs, st.Events)) != 1 {
		t.Fatal("precondition: one result owed")
	}
	return st, id
}

// Viewing a session never claims or writes it, even one owing a result: the
// result is queued once a message claims it, not when it is opened.
func TestViewingWritesNothing(t *testing.T) {
	st, id := drainedWithResultOwed(t)
	b := newBGServer(t, st)
	before, _ := st.Events(id)
	claims := st.claims
	live, err := b.s.resumeSession(context.Background(), id, "", "alice", "acme", false)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond) // past the idle settle window
	after, _ := st.Events(id)
	st.mu.Lock()
	claimed := st.claims != claims
	st.mu.Unlock()
	if len(after) != len(before) || claimed || !live.unclaimed.Load() || live.Loop.Background.Pending() != 0 {
		t.Fatalf("viewing wrote %d events, claimed %v, unclaimed %v, pending %d",
			len(after)-len(before), claimed, live.unclaimed.Load(), live.Loop.Background.Pending())
	}
	// A message claims it, and the owed result is delivered then, once.
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"what happened?"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("continue: %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "the notice", func() bool { return countType(b.events(id), agent.EvSubagentNotice) == 1 })
	time.Sleep(2500 * time.Millisecond)
	if n := countType(b.events(id), agent.EvSubagentNotice); n != 1 {
		t.Fatalf("%d notices, want 1", n)
	}
}

// Lock order: a write under the run lock never claims, so a claim in
// progress (holding claimMu) can always take the run lock. Before, an idle
// delivery on a viewed session held the run lock waiting for claimMu while
// the claim waited for the run lock.
func TestClaimAndIdleDeliveryDoNotDeadlock(t *testing.T) {
	st, id := drainedWithResultOwed(t)
	b := newBGServer(t, st)
	live, err := b.s.resumeSession(context.Background(), id, "", "alice", "acme", false)
	if err != nil {
		t.Fatal(err)
	}
	live.claimMu.Lock() // a message's claim under way
	time.Sleep(2500 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		live.Loop.SetHistory(nil, 0) // what catchUp does under claimMu
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: the run lock is held by a write waiting to claim")
	}
	live.claimMu.Unlock()
}

// Deleting or draining a session with a child running stops its heartbeat
// and releases its claim: nothing refreshes a session that is gone.
func TestDeleteAndDrainStopTheHeartbeat(t *testing.T) {
	old := nodeHeartbeat
	nodeHeartbeat = 20 * time.Millisecond
	defer func() { nodeHeartbeat = old }()
	for _, how := range []string{"delete", "drain"} {
		rm := &routingMem{MemStore: agent.NewMemStore()}
		b := newBGServer(t, rm, "one")
		b.s.opts.DrainTimeout = 50 * time.Millisecond
		id := b.start("bg:one", false)
		<-b.ended
		n, _ := rm.snapshot()
		waitUntil(t, "refreshes", func() bool { m, _ := rm.snapshot(); return m >= n+2 })
		if how == "delete" {
			if rec := b.do("alice", "DELETE", "/v1/sessions/"+id, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("delete: %d %s", rec.Code, rec.Body)
			}
		} else {
			b.s.drain()
		}
		time.Sleep(60 * time.Millisecond) // a beat already under way lands
		n, rel := rm.snapshot()
		time.Sleep(200 * time.Millisecond)
		if m, _ := rm.snapshot(); m != n || !rel {
			t.Fatalf("%s: %d refreshes after it, released %v", how, m-n, rel)
		}
		b.ad.release("one")
	}
}

// A background subagent's ask waiting with no run live is answered only by a
// request naming its request_id; an approve naming none is refused and the
// ask keeps waiting.
func TestIdleSubagentAskNeedsItsRequestID(t *testing.T) {
	b := newBGServer(t, nil, "one")
	id := b.start("bg:one", false)
	<-b.ended
	live := b.live(id)
	got := make(chan bool, 1)
	go func() {
		ctx := agent.WithRequestID(agent.WithSubagent(context.Background(), "one"), "ev-child-1")
		ok, _ := live.Approve(ctx, "bash", json.RawMessage(`{"command":"x"}`), policyAsk())
		got <- ok
	}()
	waitUntil(t, "the ask", func() bool { return b.state(id) == "waiting_approval" })
	rid := "ev-child-1"
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/approve", `{"approved":true}`); rec.Code != http.StatusConflict {
		t.Fatalf("an approve naming no request answered an idle subagent's ask: %d %s", rec.Code, rec.Body)
	}
	select {
	case <-got:
		t.Fatal("the ask was answered")
	case <-time.After(100 * time.Millisecond):
	}
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/approve", `{"approved":true,"request_id":"`+rid+`"}`); rec.Code >= 300 {
		t.Fatalf("its own request id: %d %s", rec.Code, rec.Body)
	}
	if !<-got {
		t.Fatal("the answer by request id was not applied")
	}

	// With a run live, a subagent's ask is the run's to answer, as before:
	// a client naming no request still answers it.
	live.mu.Lock()
	live.ran = make(chan struct{})
	live.mu.Unlock()
	go func() {
		ctx := agent.WithRequestID(agent.WithSubagent(context.Background(), "one"), "ev-child-2")
		ok, _ := live.Approve(ctx, "bash", json.RawMessage(`{"command":"y"}`), policyAsk())
		got <- ok
	}()
	waitUntil(t, "the second ask", func() bool { return b.state(id) == "waiting_approval" })
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/approve", `{"approved":true}`); rec.Code >= 300 {
		t.Fatalf("a live run's subagent ask with no request id: %d %s", rec.Code, rec.Body)
	}
	if !<-got {
		t.Fatal("the answer was not applied")
	}
	live.mu.Lock()
	live.ran = nil
	live.mu.Unlock()
	b.ad.release("one")
}

// At startup, a session a crashed process left open is reconciled once its
// holder's heartbeat is stale; one whose holder is alive is left alone.
func TestStartupSweepRecoversOrphans(t *testing.T) {
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	a := newBGServer(t, st, "one", "two")
	crashed := a.start("bg:one", false)
	<-a.ended
	alive := a.start("bg:two", false)
	<-a.ended
	st.crash(crashed) // its process stopped heartbeating
	if n := a.s.RecoverOrphans(context.Background()); n != 0 {
		t.Fatalf("a process reconciled %d session(s) it is running itself", n)
	}

	b := newBGServer(t, st)
	if n := b.s.RecoverOrphans(context.Background()); n != 1 {
		t.Fatalf("recovered %d, want 1", n)
	}
	st.mu.Lock()
	crashedEnded, aliveEnded := st.ended[crashed], st.ended[alive]
	st.mu.Unlock()
	if !crashedEnded || aliveEnded {
		t.Fatalf("crashed ended %v, alive ended %v", crashedEnded, aliveEnded)
	}
	var lost bool
	for _, r := range payloadsOf(b.events(crashed), agent.EvSubagentReturn) {
		lost = lost || r["reason"] == string(agent.TermLost)
	}
	if !lost || countType(b.events(alive), agent.EvSubagentReturn) != 0 {
		t.Fatal("the crashed session's task was not recorded lost, or the live one's was touched")
	}
	if st.holderOf(crashed) != "" {
		t.Fatalf("the sweep kept holding a session it ended: %q", st.holderOf(crashed))
	}
	if b.s.RecoverOrphans(context.Background()) != 0 {
		t.Fatal("a second sweep recovered again")
	}
	a.ad.release("one")
	a.ad.release("two")
}

// A fenced session writes nothing more to its record, and leaves the process
// with its tasks stopped as lease_lost.
func TestFencedSessionWritesNothing(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
		b := newBGServer(t, st, "one")
		id := b.start("bg:one", false)
		<-b.ended
		live := b.live(id)
		if resumed {
			b.ad.release("one")
			waitUntil(t, "the closing end", func() bool { e, _ := agent.LastEnd(b.events(id)); return e.Settled })
			b.s.mu.Lock()
			delete(b.s.running, id)
			b.s.mu.Unlock()
			var err error
			if live, err = b.s.resumeSession(context.Background(), id, "", "alice", "acme", true); err != nil {
				t.Fatal(err)
			}
		}
		b.s.fence(live)
		if _, err := live.Loop.Recorder.Record(agent.EvAgentMessage, agent.ActorAgent, agent.Trusted, agent.Message{Text: "late"}); !errors.Is(err, errLeaseLost) {
			t.Fatalf("resumed %v: a fenced session's write: %v", resumed, err)
		}
		if b.live(id) != nil || live.Loop.Background.Live() != 0 {
			t.Fatalf("resumed %v: still here after the fence", resumed)
		}
		if !resumed {
			b.ad.release("one")
		}
	}
}

// A started session's row carries its holder from the moment it is written.
func TestStartedSessionRowCarriesItsHolder(t *testing.T) {
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	b := newBGServer(t, st)
	id := b.start("hello", false)
	<-b.ended
	st.mu.Lock()
	holder := st.rows[id].Holder
	st.mu.Unlock()
	if holder != b.s.holder {
		t.Fatalf("the row was written with holder %q, want %q", holder, b.s.holder)
	}
}

// A store that keeps refusing a result while idle does not leave the session
// held: once the retries are spent the work owed is settled, the session is
// done here and its claim released; the result is rebuilt from the record.
func TestIdleStoreErrorDoesNotHoldTheSession(t *testing.T) {
	old := agent.IdleRetries
	agent.IdleRetries = 0
	t.Cleanup(func() { agent.IdleRetries = old })
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	st.refuseNotices.Store(true)
	b := newBGServer(t, st, "one")
	id := b.start("bg:one", false)
	<-b.ended
	b.ad.release("one")
	waitUntil(t, "the session done", func() bool { return b.state(id) == "done" })
	waitUntil(t, "the claim released", func() bool { return st.holderOf(id) == "" })
	if pend := agent.PendingNotices(b.events(id), st.Events); len(pend) != 1 {
		t.Fatalf("the result is not owed in the record: %+v", pend)
	}
}

// A node restarted with the same node id takes back the sessions it held
// before at once, without waiting out its own old heartbeat.
func TestSameNodeReclaimsItsCrashedSessions(t *testing.T) {
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	nodeA := func(_ *config.Config, o *Options) { o.NodeID = "node-a" }
	a := newBGServerWith(t, st, nodeA, "one")
	id := a.start("bg:one", false)
	<-a.ended // node-a "crashes" here, its heartbeat still fresh
	restarted := newBGServerWith(t, st, nodeA)
	other := newBGServerWith(t, st, func(_ *config.Config, o *Options) { o.NodeID = "node-b" })
	if rec := other.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"x"}`); rec.Code != http.StatusConflict {
		t.Fatalf("another node took a freshly held session: %d", rec.Code)
	}
	if rec := restarted.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"x"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("the restarted node could not take back its own session: %d %s", rec.Code, rec.Body)
	}
	a.ad.release("one")
}

// The sweep runs again every interval: a session whose holder goes stale
// after the first sweep is recovered by a later one.
func TestSweepRunsAgain(t *testing.T) {
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	a := newBGServer(t, st, "one")
	id := a.start("bg:one", false)
	<-a.ended
	b := newBGServer(t, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.s.sweepOrphans(ctx, 50*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	st.mu.Lock()
	swept := st.ended[id]
	st.mu.Unlock()
	if swept {
		t.Fatal("a live session was swept")
	}
	st.crash(id)
	waitUntil(t, "a later sweep", func() bool { st.mu.Lock(); defer st.mu.Unlock(); return st.ended[id] })
	a.ad.release("one")
}
