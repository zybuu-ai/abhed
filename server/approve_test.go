package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/store"
)

// answerWhenAsked answers the next request the session publishes, as a
// reviewer on this node would.
func answerWhenAsked(l *liveSession, approved bool, scope string) {
	p := waitAsked(l, "")
	<-p.ready
	l.mu.Lock()
	l.move(p, askAnswered, approved, scope)
	l.mu.Unlock()
}

// waitAsked returns the request once published; rid "" takes any.
func waitAsked(l *liveSession, rid string) *pendingApproval {
	for {
		l.mu.Lock()
		p := l.pending
		l.mu.Unlock()
		if p != nil && (rid == "" || p.RequestID == rid) {
			return p
		}
		time.Sleep(time.Millisecond)
	}
}

// TestApproveRemembersAlwaysAllow verifies that a reply carrying a scope is
// remembered for the session, so a later call matching the same scope is
// auto-approved without raising another prompt — the fix for default mode
// re-prompting on every mutating call.
func TestApproveRemembersAlwaysAllow(t *testing.T) {
	live := &liveSession{allowed: map[string]bool{}}
	res := policy.Result{Scope: "bash(npm install *)"}

	// First call: reviewer chooses "always allow", which sends the scope back.
	go answerWhenAsked(live, true, res.Scope)
	ok, err := live.Approve(context.Background(), "bash", nil, res)
	if err != nil || !ok {
		t.Fatalf("first approval should accept: ok=%v err=%v", ok, err)
	}
	if !live.allowed[res.Scope] {
		t.Fatalf("scope was not remembered")
	}

	// Second call with the same scope must auto-allow. Nobody answers, so if
	// Approve asked, the test would block — proving it did not ask.
	ok, err = live.Approve(context.Background(), "bash", nil, res)
	if err != nil || !ok {
		t.Fatalf("remembered scope should auto-approve: ok=%v err=%v", ok, err)
	}
	if live.State == "waiting_approval" {
		t.Fatalf("auto-approved call must not enter waiting_approval")
	}
}

// TestApproveWithoutScopeIsNotRemembered ensures a plain one-off approval does
// not widen into a standing allowance.
func TestApproveWithoutScopeIsNotRemembered(t *testing.T) {
	live := &liveSession{allowed: map[string]bool{}}
	res := policy.Result{Scope: "bash(npm install *)"}
	go answerWhenAsked(live, true, "") // no scope: approve once only
	if ok, err := live.Approve(context.Background(), "bash", nil, res); err != nil || !ok {
		t.Fatalf("approval should accept: ok=%v err=%v", ok, err)
	}
	if live.allowed[res.Scope] {
		t.Fatalf("a scopeless approval must not be remembered as always-allow")
	}
}

// approvalRows is a store holding one row per request, like Postgres.
type approvalRows struct {
	EventStore
	mu      sync.Mutex
	n       int
	rows    map[string]*bool
	askHook func(ctx context.Context) error
	ansHook func()
}

func newApprovalRows(es EventStore) *approvalRows {
	return &approvalRows{EventStore: es, rows: map[string]*bool{}}
}

func (r *approvalRows) AskApproval(ctx context.Context, _ store.Approval) (string, error) {
	if r.askHook != nil {
		if err := r.askHook(ctx); err != nil {
			return "", err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	id := fmt.Sprintf("ap-%d", r.n)
	r.rows[id] = nil
	return id, nil
}

func (r *approvalRows) AnswerApproval(_ context.Context, id string, ok bool, _ string) (bool, error) {
	r.mu.Lock()
	v, found := r.rows[id]
	if found && v == nil {
		r.rows[id] = &ok
	}
	r.mu.Unlock()
	if r.ansHook != nil {
		r.ansHook()
	}
	return found && v == nil, nil
}

func (r *approvalRows) ApprovalResult(_ context.Context, id string) (bool, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v := r.rows[id]; v != nil {
		return *v, true, nil
	}
	return false, false, nil
}

func (r *approvalRows) PendingApproval(context.Context, string) (store.Approval, bool, error) {
	return store.Approval{}, false, nil
}

func (r *approvalRows) row(id string) *bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rows[id]
}

type turnResult struct {
	ok  bool
	err error
}

// approvalSession opens a session; with rows, its approvals are recorded there.
func approvalSession(t *testing.T, rows bool) (http.Handler, *liveSession, *approvalRows, string) {
	t.Helper()
	s := testServer(t)
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"workbench":true}`)))
	var created createResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.SessionID == "" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	s.mu.RLock()
	live := s.running[created.SessionID]
	s.mu.RUnlock()
	var r *approvalRows
	if rows {
		r = newApprovalRows(s.store)
		s.store = r
		live.durable = r
	}
	return h, live, r, created.SessionID
}

// askTurn starts the turn waiting on request rid.
func askTurn(ctx context.Context, live *liveSession, rid string) chan turnResult {
	ch := make(chan turnResult, 1)
	go func() {
		ok, err := live.Approve(agent.WithRequestID(ctx, rid), "write", nil, policy.Result{})
		ch <- turnResult{ok, err}
	}()
	return ch
}

func postApprove(h http.Handler, id, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions/"+id+"/approve", strings.NewReader(body)))
	return rec
}

func approve(h http.Handler, id, body string) int { return postApprove(h, id, body).Code }

func silent(t *testing.T, c chan turnResult, why string) {
	t.Helper()
	select {
	case res := <-c:
		t.Fatalf("%s: the turn returned %+v", why, res)
	case <-time.After(100 * time.Millisecond):
	}
}

// An answer naming the pending request is taken by the turn and recorded.
func TestApproveMatchingRequestID(t *testing.T) {
	for _, rows := range []bool{false, true} {
		h, live, r, id := approvalSession(t, rows)
		c := askTurn(context.Background(), live, "ev-a")
		<-waitAsked(live, "ev-a").ready
		if code := approve(h, id, `{"approved":true,"request_id":"ev-a"}`); code != http.StatusNoContent {
			t.Fatalf("rows=%v matching request_id = %d, want 204", rows, code)
		}
		if res := <-c; !res.ok || res.err != nil {
			t.Fatalf("rows=%v turn = %+v", rows, res)
		}
		if r != nil && (r.row("ap-1") == nil || !*r.row("ap-1")) {
			t.Fatal("the answer was not recorded on its row")
		}
	}
}

// An answer given for another request must not answer the one pending: it
// is refused, and neither taken nor recorded.
func TestApproveMismatchedRequestIDIsRefused(t *testing.T) {
	for _, rows := range []bool{false, true} {
		h, live, r, id := approvalSession(t, rows)
		ctx, cancel := context.WithCancel(context.Background())
		c := askTurn(ctx, live, "ev-new")
		<-waitAsked(live, "ev-new").ready
		if code := approve(h, id, `{"approved":true,"request_id":"ev-old"}`); code != http.StatusConflict {
			t.Fatalf("rows=%v stale request_id = %d, want 409", rows, code)
		}
		silent(t, c, "a stale answer")
		if r != nil && r.row("ap-1") != nil {
			t.Fatal("a stale answer was recorded")
		}
		cancel()
		<-c
	}
}

// A client that sends no request_id answers the pending request, as before.
func TestApproveWithoutRequestIDAnswersThePendingOne(t *testing.T) {
	h, live, _, id := approvalSession(t, false)
	c := askTurn(context.Background(), live, "ev-a")
	<-waitAsked(live, "ev-a").ready
	if code := approve(h, id, `{"approved":false}`); code != http.StatusNoContent {
		t.Fatalf("no request_id = %d, want 204", code)
	}
	if res := <-c; res.ok || res.err != nil {
		t.Fatalf("turn = %+v, want a plain denial", res)
	}
}

// A's answer, written while A is interrupted and B published, lands on A's
// row and is reported as recorded but not applied; B is untouched.
func TestApproveAnswersItsOwnRowNotTheNewest(t *testing.T) {
	h, live, r, id := approvalSession(t, true)
	ctxA, cancelA := context.WithCancel(context.Background())
	a := askTurn(ctxA, live, "ev-a")
	<-waitAsked(live, "ev-a").ready
	var b chan turnResult
	r.ansHook = func() {
		r.ansHook = nil
		cancelA()
		<-a
		b = askTurn(context.Background(), live, "ev-b")
		<-waitAsked(live, "ev-b").ready
	}
	rec := postApprove(h, id, `{"approved":true,"request_id":"ev-a"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"recorded":true`) || !strings.Contains(rec.Body.String(), `"applied":false`) {
		t.Fatalf("A's answer = %d %s, want 200 recorded, not applied", rec.Code, rec.Body)
	}
	if r.row("ap-2") != nil {
		t.Fatal("A's answer was recorded on B's row")
	}
	silent(t, b, "A's answer")
	if code := approve(h, id, `{"approved":false,"request_id":"ev-b"}`); code != http.StatusNoContent {
		t.Fatalf("B's own answer = %d", code)
	}
	if res := <-b; res.ok {
		t.Fatal("B approved on its own denial")
	}
}

// Allow and deny at once to a real waiting turn, with and without a store:
// one is taken with 204, the other refused, and the turn hears the one taken.
func TestConcurrentAnswersAgree(t *testing.T) {
	for _, rows := range []bool{false, true} {
		for i := 0; i < 25; i++ {
			h, live, r, id := approvalSession(t, rows)
			c := askTurn(context.Background(), live, "ev-a")
			<-waitAsked(live, "ev-a").ready
			codes := make([]int, 2)
			var wg sync.WaitGroup
			for j, body := range []string{`{"approved":true,"request_id":"ev-a"}`, `{"approved":false,"request_id":"ev-a"}`} {
				wg.Add(1)
				go func(j int, body string) { defer wg.Done(); codes[j] = approve(h, id, body) }(j, body)
			}
			wg.Wait()
			res := <-c
			taken := -1
			for j, code := range codes {
				switch code {
				case http.StatusNoContent:
					if taken >= 0 {
						t.Fatalf("rows=%v both answers were taken: %v", rows, codes)
					}
					taken = j
				case http.StatusConflict:
				default:
					t.Fatalf("rows=%v codes %v", rows, codes)
				}
			}
			if taken < 0 || res.ok != (taken == 0) {
				t.Fatalf("rows=%v codes %v, the turn heard approved=%v", rows, codes, res.ok)
			}
			if r != nil && (r.row("ap-1") == nil || *r.row("ap-1") != res.ok) {
				t.Fatalf("the row disagrees with the turn")
			}
		}
	}
}

// An answer naming a request that has not been published yet waits for it:
// the event reaches the client just before the turn starts waiting.
func TestApproveWaitsForTheNamedRequest(t *testing.T) {
	h, live, _, id := approvalSession(t, false)
	started := make(chan chan turnResult, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		started <- askTurn(context.Background(), live, "ev-a")
	}()
	if code := approve(h, id, `{"approved":true,"request_id":"ev-a"}`); code != http.StatusNoContent {
		t.Fatalf("an early answer = %d, want 204", code)
	}
	if res := <-<-started; !res.ok {
		t.Fatal("the turn did not hear the early answer")
	}
}

// The turn's poll can read the row before the answer's write returns. The
// answer wrote it, so it is told the turn took it, not "applied": false.
func TestApproveTakenFromTheRowIsReportedTaken(t *testing.T) {
	h, live, r, id := approvalSession(t, true)
	r.ansHook = func() { time.Sleep(approvalPoll + 500*time.Millisecond) }
	c := askTurn(context.Background(), live, "ev-a")
	<-waitAsked(live, "ev-a").ready
	if code := approve(h, id, `{"approved":true,"request_id":"ev-a"}`); code != http.StatusNoContent {
		t.Fatalf("answer = %d, want 204", code)
	}
	if res := <-c; !res.ok {
		t.Fatal("the turn did not run the approval")
	}
}

// Interrupted while the row is being written, with answers waiting on it:
// none is told the turn took it, and the turn reports the interrupt.
func TestInterruptWhileTheRowIsWritten(t *testing.T) {
	for i := 0; i < 20; i++ {
		h, live, r, id := approvalSession(t, true)
		r.askHook = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
		ctx, cancel := context.WithCancel(context.Background())
		c := askTurn(ctx, live, "ev-a")
		waitAsked(live, "ev-a")
		codes := make([]int, 5)
		var wg sync.WaitGroup
		for j := range codes {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				codes[j] = approve(h, id, fmt.Sprintf(`{"approved":%v,"request_id":"ev-a"}`, j%2 == 0))
			}(j)
		}
		time.Sleep(20 * time.Millisecond)
		cancel()
		wg.Wait()
		res := <-c
		if res.err == nil {
			t.Fatalf("the turn did not report the interrupt: %+v", res)
		}
		for _, code := range codes {
			if code != http.StatusConflict {
				t.Fatalf("codes %v, want every answer refused", codes)
			}
		}
	}
}

// A request is answerable while its row is still being written, longer
// than the publish wait: the answer waits for the row and is recorded on it.
func TestAnswerWaitsForASlowRow(t *testing.T) {
	h, live, r, id := approvalSession(t, true)
	release := make(chan struct{})
	r.askHook = func(context.Context) error { <-release; return nil }
	c := askTurn(context.Background(), live, "ev-a")
	code := make(chan int, 1)
	go func() { code <- approve(h, id, `{"approved":true,"request_id":"ev-a"}`) }()
	select {
	case got := <-code:
		t.Fatalf("answered %d before the row existed", got)
	case <-time.After(requestWait + 500*time.Millisecond):
	}
	close(release)
	if got := <-code; got != http.StatusNoContent {
		t.Fatalf("answer = %d, want 204", got)
	}
	if res := <-c; !res.ok {
		t.Fatal("the turn did not hear the approval")
	}
	if v := r.row("ap-1"); v == nil || !*v {
		t.Fatal("the answer was not recorded on the row")
	}
}

// Requests that stopped waiting are remembered oldest first out, so a late
// answer to a recent one is refused at once.
func TestEndedRequestsAreAFIFO(t *testing.T) {
	live := &liveSession{allowed: map[string]bool{}}
	for i := 0; i < endedKept+6; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		c := askTurn(ctx, live, fmt.Sprintf("ev-%d", i))
		waitAsked(live, fmt.Sprintf("ev-%d", i))
		cancel()
		<-c
	}
	if len(live.ended) != endedKept || live.ended["ev-5"] || !live.ended["ev-6"] || !live.ended[fmt.Sprintf("ev-%d", endedKept+5)] {
		t.Fatalf("ended holds %d; ev-5 %v ev-6 %v", len(live.ended), live.ended["ev-5"], live.ended["ev-6"])
	}
}

// Answers waiting for a request are capped per session.
func TestParkedAnswersAreCapped(t *testing.T) {
	h, _, _, id := approvalSession(t, false)
	recs := make([]*httptest.ResponseRecorder, maxParked+4)
	var wg sync.WaitGroup
	for j := range recs {
		wg.Add(1)
		go func(j int) {
			defer wg.Done()
			recs[j] = postApprove(h, id, `{"approved":true,"request_id":"ev-never"}`)
		}(j)
	}
	wg.Wait()
	busy := 0
	for _, rec := range recs {
		if rec.Code == http.StatusTooManyRequests {
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("a busy answer was not told when to retry")
			}
			busy++
		}
	}
	if busy != 4 {
		t.Fatalf("%d answers over the cap of %d were told to retry, want 4", busy, maxParked)
	}
}

// routedApprovals is a store holding approvals and routing, for a session
// this node is not running.
type routedApprovals struct {
	*fakeApprovals
	node string
}

func (routedApprovals) ClaimNode(context.Context, string, string) error   { return nil }
func (routedApprovals) ReleaseNode(context.Context, string, string) error { return nil }
func (r routedApprovals) NodeFor(context.Context, string, time.Duration) (string, error) {
	return r.node, nil
}

func (f *fakeApprovals) answer(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.answered[id]
	return ok
}

// On a node not running the session, an answer naming a request is not put
// on the newest row, which cannot be checked; it is sent to the owning node.
// An answer naming none is recorded there, as before.
func TestApproveElsewhereLeavesABoundAnswerToTheOwner(t *testing.T) {
	f := newFakeApprovals()
	f.pending["s-far"] = store.Approval{ID: "ap-far", SessionID: "s-far"}
	s := &Server{store: routedApprovals{f, "node-b"}, opts: Options{NodeID: "node-a"},
		running: map[string]*liveSession{}, log: discardLogger()}
	send := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/sessions/s-far/approve", strings.NewReader(body))
		req.SetPathValue("id", "s-far")
		s.approveAction(rec, req)
		return rec
	}
	rec := send(`{"approved":true,"request_id":"ev-1"}`)
	if rec.Code != http.StatusMisdirectedRequest || rec.Header().Get("Abhed-Session-Node") != "node-b" {
		t.Fatalf("bound answer elsewhere = %d node %q, want 421 to node-b", rec.Code, rec.Header().Get("Abhed-Session-Node"))
	}
	if answered := f.answer("ap-far"); answered {
		t.Fatal("a bound answer was recorded on a row it could not check")
	}
	if rec := send(`{"approved":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("unbound answer elsewhere = %d, want 204", rec.Code)
	}
	if answered := f.answer("ap-far"); !answered {
		t.Fatal("an unbound answer elsewhere was not recorded")
	}
}

// "Always allow" remembers only the scope the request offered: a wider one
// sent by the client is refused, and the offered one is taken.
func TestApproveRefusesAScopeTheRequestDidNotOffer(t *testing.T) {
	h, live, _, id := approvalSession(t, false)
	c := make(chan turnResult, 1)
	go func() {
		ok, err := live.Approve(agent.WithRequestID(context.Background(), "ev-a"), "bash", nil, policy.Result{Scope: "bash(ls *)"})
		c <- turnResult{ok, err}
	}()
	<-waitAsked(live, "ev-a").ready
	if code := approve(h, id, `{"approved":true,"request_id":"ev-a","scope":"bash(*)"}`); code != http.StatusBadRequest {
		t.Fatalf("a wider scope = %d, want 400", code)
	}
	silent(t, c, "a refused scope")
	if code := approve(h, id, `{"approved":true,"request_id":"ev-a","scope":"bash(ls *)"}`); code != http.StatusNoContent {
		t.Fatalf("the offered scope = %d, want 204", code)
	}
	if res := <-c; !res.ok {
		t.Fatal("the turn did not take the approval")
	}
	live.mu.Lock()
	defer live.mu.Unlock()
	if live.allowed["bash(*)"] || !live.allowed["bash(ls *)"] {
		t.Fatalf("allowed = %v, want only the offered scope", live.allowed)
	}
}
