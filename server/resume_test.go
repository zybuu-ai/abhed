package server

import (
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
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// durableMem is the memory store plus the two things a durable store gives
// the server: session rows it can list, and an atomic claim on a finished
// session. It is what Postgres does, in a map.
type durableMem struct {
	*agent.MemStore
	mu     sync.Mutex
	rows   map[string]store.SessionRecord
	ended  map[string]bool
	claims int
	// onClaim, when set, runs after a claim is taken.
	onClaim func()
}

func (d *durableMem) CreateSession(ctx context.Context, r store.SessionRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rows[r.ID] = r
	return nil
}
func (d *durableMem) ListSessions(ctx context.Context, limit int) ([]store.SessionRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]store.SessionRecord, 0, len(d.rows))
	for id, r := range d.rows {
		if r.ParentID != "" {
			continue // as Postgres lists: subagents are reached through their parent
		}
		if d.ended[id] {
			at := time.Now()
			r.EndedAt = &at
		}
		out = append(out, r)
	}
	return out, nil
}

// GetSession finds one row by id, subagents' included, as Postgres does.
func (d *durableMem) GetSession(ctx context.Context, id string) (store.SessionRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.rows[id]
	if !ok {
		return r, store.ErrNotFound
	}
	if d.ended[id] {
		at := time.Now()
		r.EndedAt = &at
	}
	return r, nil
}
func (d *durableMem) Append(ev agent.Event) error {
	if ev.Type == agent.EvSessionEnded {
		d.mu.Lock()
		d.ended[ev.SessionID] = true
		d.mu.Unlock()
	}
	return d.MemStore.Append(ev)
}
func (d *durableMem) ClaimResume(ctx context.Context, id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.rows[id]; !ok || !d.ended[id] {
		return false, nil
	}
	d.ended[id] = false
	d.claims++
	if d.onClaim != nil {
		d.onClaim()
	}
	return true, nil
}

// A session that ended with the process that ran it used to be a dead end:
// "session not found — it may have ended with this server process". The
// record is the conversation, so a finished session is continued from it —
// by this process after a restart, or by another node — with one monotonic
// event sequence and the original owner still the only one who can.
func TestFinishedSessionContinuesFromRecord(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}, tools.Glob{}), Store: st})
	h := s.Handler()

	do := func(method, path, user, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.Header.Set("X-Abhed-User", user)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	rec := do("POST", "/v1/sessions", "alice", `{"prompt":"first question"}`)
	var out struct {
		SessionID string `json:"session_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.SessionID == "" {
		t.Fatalf("no session: %s", rec.Body.String())
	}
	// Let the stub adapter finish the run and the process forget it.
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.RLock()
		live := s.running[out.SessionID]
		s.mu.RUnlock()
		live.mu.Lock()
		done := live.State == "done"
		live.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.mu.Lock()
	delete(s.running, out.SessionID) // as a restart, or another node, would see it
	s.mu.Unlock()

	var before []json.RawMessage
	json.Unmarshal(do("GET", "/v1/sessions/"+out.SessionID+"/replay", "alice", "").Body.Bytes(), &before)
	if len(before) == 0 {
		t.Fatal("no events recorded for the first run")
	}

	// Another user cannot continue it.
	if w := do("POST", "/v1/sessions/"+out.SessionID+"/messages", "bob", `{"prompt":"mine now"}`); w.Code != http.StatusNotFound {
		t.Fatalf("bob continued alice's session: %d %s", w.Code, w.Body.String())
	}
	// The owner can, and the process that forgot it picks it up from the record.
	w := do("POST", "/v1/sessions/"+out.SessionID+"/messages", "alice", `{"prompt":"second question"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("owner could not continue a finished session: %d %s", w.Code, w.Body.String())
	}
	time.Sleep(300 * time.Millisecond)

	var after []struct {
		Seq  int64  `json:"seq"`
		Type string `json:"type"`
	}
	json.Unmarshal(do("GET", "/v1/sessions/"+out.SessionID+"/replay", "alice", "").Body.Bytes(), &after)
	if len(after) <= len(before) {
		t.Fatalf("continuation recorded nothing: %d events before, %d after", len(before), len(after))
	}
	for i := 1; i < len(after); i++ {
		if after[i].Seq != after[i-1].Seq+1 {
			t.Fatalf("sequence broke at %d → %d: a continuation must extend the record, not restart it", after[i-1].Seq, after[i].Seq)
		}
	}
	found := false
	for _, ev := range after[len(before):] {
		if ev.Type == "user.message" {
			found = true
		}
	}
	if !found {
		t.Error("the second question was not recorded in the same session")
	}

	// While it is running here, a second continuer — the same request landing
	// on another node — is refused, not doubled.
	if w := do("POST", "/v1/sessions/"+out.SessionID+"/messages", "alice", `{"prompt":"again"}`); w.Code == http.StatusNotFound {
		t.Fatalf("a running continuation went missing: %d %s", w.Code, w.Body.String())
	}
	time.Sleep(300 * time.Millisecond)
	s.mu.Lock()
	delete(s.running, out.SessionID) // this node forgets it…
	s.mu.Unlock()
	st.mu.Lock()
	st.ended[out.SessionID] = false // …while the store says another node holds it
	st.mu.Unlock()
	if w := do("POST", "/v1/sessions/"+out.SessionID+"/messages", "alice", `{"prompt":"third"}`); w.Code != http.StatusConflict {
		t.Fatalf("a session claimed elsewhere was continued twice: %d %s", w.Code, w.Body.String())
	}
}

// viewRig is a server whose one finished session, not running on this node, is
// then opened at the workbench only to view it.
type viewRig struct {
	t  *testing.T
	s  *Server
	st *durableMem
	id string
	do func(method, path, body string) *httptest.ResponseRecorder
}

func newViewRig(t *testing.T) *viewRig {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}, tools.Glob{}), Store: st})
	h := s.Handler()
	v := &viewRig{t: t, s: s, st: st}
	v.do = func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Abhed-User", "alice")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	json.Unmarshal(v.do("POST", "/v1/sessions", `{"prompt":"first question"}`).Body.Bytes(), &out)
	v.id = out.SessionID
	for deadline := time.Now().Add(3 * time.Second); !v.ended() && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	s.mu.Lock()
	delete(s.running, v.id) // as a restart, or another node, would see it
	s.mu.Unlock()
	st.mu.Lock()
	st.claims = 0
	st.mu.Unlock()
	return v
}

func (v *viewRig) ended() bool {
	v.st.mu.Lock()
	defer v.st.mu.Unlock()
	return v.st.ended[v.id]
}

func (v *viewRig) claims() int {
	v.st.mu.Lock()
	defer v.st.mu.Unlock()
	return v.st.claims
}

// view opens the session as the workbench does on view; the accept call
// reaches it the same way and changes nothing.
func (v *viewRig) view() *httptest.ResponseRecorder {
	return v.do("POST", "/v1/sessions/"+v.id+"/accept", `{"path":"none.txt","content":""}`)
}

func (v *viewRig) live() *liveSession {
	v.s.mu.RLock()
	defer v.s.mu.RUnlock()
	return v.s.running[v.id]
}

// checkSeqs fails unless the record's steps are 1..n, each once.
func (v *viewRig) checkSeqs() []agent.Event {
	events, _ := v.st.Events(v.id)
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			v.t.Fatalf("step %d holds seq %d", i+1, ev.Seq)
		}
	}
	return events
}

// Opening a finished session at the workbench, only to view it, leaves its
// stored end in place; a message then claims it in the store.
func TestViewingAFinishedSessionLeavesItEnded(t *testing.T) {
	v := newViewRig(t)
	if w := v.view(); w.Code == http.StatusConflict || v.live() == nil {
		t.Fatalf("the finished session could not be opened: %d %s", w.Code, w.Body.String())
	}
	if !v.ended() || v.claims() != 0 {
		t.Fatal("viewing the session claimed it in the store")
	}
	if w := v.do("POST", "/v1/sessions/"+v.id+"/messages", `{"prompt":"second question"}`); w.Code != http.StatusAccepted {
		t.Fatalf("a message to the viewed session: %d %s", w.Code, w.Body.String())
	}
	if v.claims() != 1 {
		t.Fatalf("a message ran after %d claims, want 1", v.claims())
	}
}

// A session another node is running is not opened here, even to view.
func TestViewingASessionRunningElsewhereIsRefused(t *testing.T) {
	v := newViewRig(t)
	if claimed, _ := v.st.ClaimResume(context.Background(), v.id); !claimed {
		t.Fatal("rig: could not claim")
	}
	if w := v.view(); w.Code != http.StatusConflict {
		t.Fatalf("opened a session running elsewhere: %d %s", w.Code, w.Body.String())
	}
}

// A viewer's write waits for the claim: refused while another process runs
// the session, and after that process ends it, written after its steps with
// the conversation caught up, so neither side loses or clashes a step.
func TestViewerWriteClaimsAndCatchesUp(t *testing.T) {
	v := newViewRig(t)
	v.view()
	live := v.live()
	before, _ := v.st.Events(v.id)

	// Another process continues the session.
	if claimed, _ := v.st.ClaimResume(context.Background(), v.id); !claimed {
		t.Fatal("rig: could not claim")
	}
	other := agent.NewRecorder(v.st, v.id, "")
	other.Advance(before[len(before)-1].Seq)
	if _, err := other.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "OSPREY-58"}); err != nil {
		t.Fatal(err)
	}
	if _, err := live.Loop.Recorder.Record(agent.EvChangeAccepted, agent.ActorUser, agent.Trusted, map[string]any{"path": "a"}); !errors.Is(err, errBusySession) {
		t.Fatalf("a viewer wrote while another process ran the session: %v", err)
	}
	if _, err := other.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted, Turns: 2, TokensIn: 99}); err != nil {
		t.Fatal(err)
	}

	if _, err := live.Loop.Recorder.Record(agent.EvChangeAccepted, agent.ActorUser, agent.Trusted, map[string]any{"path": "a"}); err != nil {
		t.Fatalf("the viewer's write after the other process ended: %v", err)
	}
	events := v.checkSeqs()
	if n := len(events); n != len(before)+3 || events[n-1].Type != agent.EvChangeAccepted {
		t.Fatalf("record has %d events ending %s; want %d ending the viewer's write", n, events[n-1].Type, len(before)+3)
	}
	msgs, _ := json.Marshal(live.Loop.Messages())
	if !strings.Contains(string(msgs), "OSPREY-58") || live.Loop.Usage().InputTokens != 99 {
		t.Fatalf("the conversation was not caught up: tokens %d, %s", live.Loop.Usage().InputTokens, msgs)
	}
}

// A claim taken for workbench work alone is released with the end the
// session was opened with, once it goes quiet.
func TestWorkbenchHoldIsReleasedAsFound(t *testing.T) {
	v := newViewRig(t)
	v.view()
	live := v.live()
	before, _ := v.st.Events(v.id)
	if _, err := live.Loop.Recorder.Record(agent.EvChangeAccepted, agent.ActorUser, agent.Trusted, map[string]any{"path": "a"}); err != nil {
		t.Fatal(err)
	}
	if v.ended() {
		t.Fatal("a workbench write did not claim the session")
	}
	v.s.releaseHeld(v.id, live, false)
	events := v.checkSeqs()
	last := events[len(events)-1]
	if !v.ended() || last.Type != agent.EvSessionEnded || string(last.Payload) != string(before[len(before)-1].Payload) {
		t.Fatalf("the hold was not released as found: ended %v, last %s %s", v.ended(), last.Type, last.Payload)
	}
	if !live.unclaimed.Load() {
		t.Fatal("a released session's next write would not claim it again")
	}
}

// A message during a drain is refused before it claims anything.
func TestMessageDuringDrainDoesNotClaim(t *testing.T) {
	v := newViewRig(t)
	v.view()
	v.s.draining.Store(true)
	if w := v.do("POST", "/v1/sessions/"+v.id+"/messages", `{"prompt":"late"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a message during a drain: %d %s", w.Code, w.Body.String())
	}
	if !v.ended() || v.claims() != 0 {
		t.Fatal("a message during a drain claimed the session")
	}
}

// A claim a message took and could not use is given back as it was found.
func TestUnusedMessageClaimIsGivenBack(t *testing.T) {
	v := newViewRig(t)
	v.view()
	live := v.live()
	live.mu.Lock()
	live.State = "running" // a turn this request cannot start
	live.ran = make(chan struct{})
	close(live.ran)
	live.mu.Unlock()
	w := v.do("POST", "/v1/sessions/"+v.id+"/messages", `{"prompt":"now","interrupt":true}`)
	if w.Code == http.StatusAccepted {
		t.Fatalf("the request started a turn on a session it could not free")
	}
	if !v.ended() || !live.unclaimed.Load() {
		t.Fatalf("an unused claim was kept (%d %s)", w.Code, w.Body.String())
	}
}

// A message to a session held for workbench work takes the hold over: once
// the hold would have expired, the turn's end is the only end recorded.
func TestMessageTakesOverAWorkbenchHold(t *testing.T) {
	v := newViewRig(t)
	hold := manualHold
	manualHold = 100 * time.Millisecond
	t.Cleanup(func() { manualHold = hold })
	v.view()
	live := v.live()
	if _, err := live.Loop.Recorder.Record(agent.EvChangeAccepted, agent.ActorUser, agent.Trusted, map[string]any{"path": "a"}); err != nil {
		t.Fatal(err)
	}
	before, _ := v.st.Events(v.id)
	if w := v.do("POST", "/v1/sessions/"+v.id+"/messages", `{"prompt":"second question"}`); w.Code != http.StatusAccepted {
		t.Fatalf("a message to the held session: %d %s", w.Code, w.Body.String())
	}
	for deadline := time.Now().Add(3 * time.Second); !v.ended() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(4 * manualHold)
	events := v.checkSeqs()
	var ends []agent.SessionEnded
	for _, ev := range events[len(before):] {
		if ev.Type == agent.EvSessionEnded {
			var end agent.SessionEnded
			_ = json.Unmarshal(ev.Payload, &end)
			ends = append(ends, end)
		}
	}
	prior, _ := agent.LastEnd(before)
	if len(ends) != 1 || ends[0].TokensIn <= prior.TokensIn {
		t.Fatalf("the message's turn left ends %+v; want one, with the turn's totals (more than %d tokens in)", ends, prior.TokensIn)
	}
}

// A drain releases a session held for workbench work alone with the end it
// was opened with, so its row does not stay running after the node exits.
func TestDrainReleasesAWorkbenchHold(t *testing.T) {
	v := newViewRig(t)
	v.view()
	live := v.live()
	before, _ := v.st.Events(v.id)
	if _, err := live.Loop.Recorder.Record(agent.EvChangeAccepted, agent.ActorUser, agent.Trusted, map[string]any{"path": "a"}); err != nil {
		t.Fatal(err)
	}
	v.s.closeIdle()
	events := v.checkSeqs()
	last := events[len(events)-1]
	if !v.ended() || last.Type != agent.EvSessionEnded || string(last.Payload) != string(before[len(before)-1].Payload) {
		t.Fatalf("the drain did not release the hold as found: ended %v, last %s %s", v.ended(), last.Type, last.Payload)
	}
}

// A drain that begins once a message has claimed a finished session gives
// the claim back, ended as it was.
func TestDrainAfterResumeClaimGivesItBack(t *testing.T) {
	v := newViewRig(t)
	v.st.onClaim = func() { v.s.draining.Store(true) }
	if w := v.do("POST", "/v1/sessions/"+v.id+"/messages", `{"prompt":"late"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a message as the drain began: %d %s", w.Code, w.Body.String())
	}
	if v.claims() != 1 || !v.ended() {
		t.Fatalf("the claim was kept: %d claims, ended %v", v.claims(), v.ended())
	}
}

// A drain writes no end for a session only opened here: its row keeps the
// end it had.
func TestDrainLeavesAnOpenedSessionAlone(t *testing.T) {
	v := newViewRig(t)
	v.view()
	live := v.live()
	live.mu.Lock()
	live.State = "idle"
	live.mu.Unlock()
	before, _ := v.st.Events(v.id)
	v.s.closeIdle()
	if after, _ := v.st.Events(v.id); len(after) != len(before) {
		t.Fatalf("the drain wrote %d events to a session only opened here", len(after)-len(before))
	}
}

// A session continued on the server keeps its token totals.
func TestServerResumeCarriesTokenTotals(t *testing.T) {
	v := newViewRig(t)
	v.view()
	if got := v.live().Loop.Usage().InputTokens; got != 10 {
		t.Fatalf("resumed with %d tokens in, want the recorded 10", got)
	}
}
