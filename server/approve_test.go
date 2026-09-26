package server

import (
	"context"
	"encoding/json"
	"errors"
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
	res := policy.Result{Decision: policy.Ask, Step: "default", Scope: "bash(npm install *)"}

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
	res := policy.Result{Decision: policy.Ask, Step: "default", Scope: "bash(npm install *)"}
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
	scopes  map[string]string
	ended   map[string]bool
	askHook func(ctx context.Context) error
	ansHook func()
}

func newApprovalRows(es EventStore) *approvalRows {
	return &approvalRows{EventStore: es, rows: map[string]*bool{}, scopes: map[string]string{}, ended: map[string]bool{}}
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

func (r *approvalRows) AnswerApproval(_ context.Context, id string, ok bool, scope, _ string) (bool, error) {
	r.mu.Lock()
	v, found := r.rows[id]
	open := found && v == nil && !r.ended[id]
	if open {
		r.rows[id], r.scopes[id] = &ok, scope
	}
	r.mu.Unlock()
	if r.ansHook != nil {
		r.ansHook()
	}
	return open, nil
}

func (r *approvalRows) ApprovalResult(_ context.Context, id string) (bool, bool, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v := r.rows[id]; v != nil {
		return *v, true, r.scopes[id], nil
	}
	return false, false, "", nil
}

func (r *approvalRows) EndApproval(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended[id] = true
	return nil
}

func (r *approvalRows) isEnded(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ended[id]
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

// storedSessions stands in for the durable session rows.
type storedSessions []store.SessionRecord

func (storedSessions) CreateSession(context.Context, store.SessionRecord) error { return nil }
func (s storedSessions) ListSessions(context.Context, int) ([]store.SessionRecord, error) {
	return s, nil
}

// farSession is s-far's row: priya's, in the default tenant.
var farSession = storedSessions{{ID: "s-far", Tenant: "default", User: "priya"}}

// asUser is a request as the middleware leaves it for a signed-in user.
func asUser(req *http.Request, tenant, user string) *http.Request {
	ctx := context.WithValue(req.Context(), ctxUser, user)
	return req.WithContext(context.WithValue(ctx, ctxTenant, tenant))
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
		sessions: farSession, running: map[string]*liveSession{}, log: discardLogger()}
	send := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := asUser(httptest.NewRequest("POST", "/v1/sessions/s-far/approve", strings.NewReader(body)), "default", "priya")
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

// On a node not running the session, only its owner may answer its pending
// approval, as on the node that runs it: another user, another tenant, or a
// session with no stored row gets no answer recorded.
func TestApproveElsewhereChecksTheOwner(t *testing.T) {
	for _, c := range []struct {
		name, tenant, user string
		rows               storedSessions
		want               bool
	}{
		{"the owner", "default", "priya", farSession, true},
		{"another user", "default", "omar", farSession, false},
		{"another tenant", "acme", "priya", farSession, false},
		{"no stored row", "default", "priya", storedSessions{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeApprovals()
			f.pending["s-far"] = store.Approval{ID: "ap-owner", SessionID: "s-far"}
			s := &Server{store: routedApprovals{f, "node-b"}, opts: Options{NodeID: "node-a"},
				sessions: c.rows, running: map[string]*liveSession{}, log: discardLogger()}
			rec := httptest.NewRecorder()
			req := asUser(httptest.NewRequest("POST", "/v1/sessions/s-far/approve",
				strings.NewReader(`{"approved":true}`)), c.tenant, c.user)
			req.SetPathValue("id", "s-far")
			s.approveAction(rec, req)
			if got := f.answer("ap-owner"); got != c.want {
				t.Fatalf("answer recorded = %v (status %d), want %v", got, rec.Code, c.want)
			}
			if !c.want && rec.Code == http.StatusNoContent {
				t.Fatal("a refused answer was reported as recorded")
			}
		})
	}
}

// gettableSessions finds a row by id that the recent list no longer holds.
type gettableSessions struct{ storedSessions }

func (gettableSessions) ListSessions(context.Context, int) ([]store.SessionRecord, error) {
	return nil, nil
}
func (g gettableSessions) GetSession(_ context.Context, id string) (store.SessionRecord, error) {
	for _, r := range g.storedSessions {
		if r.ID == id {
			return r, nil
		}
	}
	return store.SessionRecord{}, store.ErrNotFound
}

// The owner of a session older than the recent list can still answer it: the
// row is found by id.
func TestApproveElsewhereFindsAnOlderSessionById(t *testing.T) {
	f := newFakeApprovals()
	f.pending["s-far"] = store.Approval{ID: "ap-old", SessionID: "s-far"}
	s := &Server{store: routedApprovals{f, "node-b"}, opts: Options{NodeID: "node-a"},
		sessions: gettableSessions{farSession}, running: map[string]*liveSession{}, log: discardLogger()}
	answer := func(user string) int {
		rec := httptest.NewRecorder()
		req := asUser(httptest.NewRequest("POST", "/v1/sessions/s-far/approve",
			strings.NewReader(`{"approved":true}`)), "default", user)
		req.SetPathValue("id", "s-far")
		s.approveAction(rec, req)
		return rec.Code
	}
	if code := answer("omar"); code == http.StatusNoContent || f.answer("ap-old") {
		t.Fatalf("another user's answer = %d, recorded %v", code, f.answer("ap-old"))
	}
	if code := answer("priya"); code != http.StatusNoContent || !f.answer("ap-old") {
		t.Fatalf("the owner's answer = %d, recorded %v; want 204 and recorded", code, f.answer("ap-old"))
	}
}

// "Always allow" remembers only the scope the request offered: a wider one
// sent by the client is refused, and the offered one is taken.
func TestApproveRefusesAScopeTheRequestDidNotOffer(t *testing.T) {
	h, live, _, id := approvalSession(t, false)
	c := make(chan turnResult, 1)
	go func() {
		ok, err := live.Approve(agent.WithRequestID(context.Background(), "ev-a"), "bash", nil, policy.Result{Decision: policy.Ask, Step: "default", Scope: "bash(ls *)"})
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

// A request that ends without an answer closes its row, so no later answer
// is recorded on it as an approval nothing acted on.
func TestAnEndedRequestClosesItsRow(t *testing.T) {
	h, live, r, id := approvalSession(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	c := askTurn(ctx, live, "ev-a")
	<-waitAsked(live, "ev-a").ready
	cancel()
	if res := <-c; res.err == nil {
		t.Fatalf("the turn did not report the interrupt: %+v", res)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !r.isEnded("ap-1") {
		if time.Now().After(deadline) {
			t.Fatal("the interrupted request left its row open")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ok, _ := r.AnswerApproval(context.Background(), "ap-1", true, "", "late"); ok {
		t.Fatal("an ended row took an answer")
	}
	if code := approve(h, id, `{"approved":true}`); code != http.StatusConflict {
		t.Fatalf("a late answer = %d, want 409", code)
	}
	if v := r.row("ap-1"); v != nil {
		t.Fatalf("the ended row was answered: %v", *v)
	}
}

// A row written after the turn gave up on it is closed too.
func TestARowWrittenAfterTheTurnGaveUpIsClosed(t *testing.T) {
	_, live, r, _ := approvalSession(t, true)
	release := make(chan struct{})
	r.askHook = func(context.Context) error { <-release; return nil }
	ctx, cancel := context.WithCancel(context.Background())
	c := askTurn(ctx, live, "ev-a")
	waitAsked(live, "ev-a")
	cancel()
	if res := <-c; res.err == nil {
		t.Fatalf("the turn did not report the interrupt: %+v", res)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for !r.isEnded("ap-1") {
		if time.Now().After(deadline) {
			t.Fatal("the row written after the turn gave up was left open")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// On a node not running the session, an unbound answer is refused when no
// node runs it or its request has ended: nothing waits on that row.
func TestApproveElsewhereRefusesARowNothingWaitsOn(t *testing.T) {
	for _, c := range []struct {
		name  string
		node  string
		ended bool
	}{
		{"no node runs the session", "", false},
		{"its request ended", "node-b", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeApprovals()
			f.pending["s-far"] = store.Approval{ID: "ap-far", SessionID: "s-far"}
			if c.ended {
				_ = f.EndApproval(context.Background(), "ap-far")
			}
			s := &Server{store: routedApprovals{f, c.node}, opts: Options{NodeID: "node-a"},
				sessions: farSession, running: map[string]*liveSession{}, log: discardLogger()}
			rec := httptest.NewRecorder()
			req := asUser(httptest.NewRequest("POST", "/v1/sessions/s-far/approve", strings.NewReader(`{"approved":true}`)), "default", "priya")
			req.SetPathValue("id", "s-far")
			s.approveAction(rec, req)
			if rec.Code != http.StatusConflict {
				t.Fatalf("answer = %d, want 409", rec.Code)
			}
			if answered := f.answer("ap-far"); answered {
				t.Fatal("a row nothing waits on was marked answered")
			}
		})
	}
}

// An answer relayed through another node keeps its choice: "approve once"
// stays once when the owner reads it from the row.
func TestApproveElsewhereRecordsTheChosenScope(t *testing.T) {
	f := newFakeApprovals()
	f.pending["s-far"] = store.Approval{ID: "ap-far", SessionID: "s-far", Scope: "bash(ls *)"}
	s := &Server{store: routedApprovals{f, "node-b"}, opts: Options{NodeID: "node-a"},
		sessions: farSession, running: map[string]*liveSession{}, log: discardLogger()}
	send := func(body string) int {
		rec := httptest.NewRecorder()
		req := asUser(httptest.NewRequest("POST", "/v1/sessions/s-far/approve", strings.NewReader(body)), "default", "priya")
		req.SetPathValue("id", "s-far")
		s.approveAction(rec, req)
		return rec.Code
	}
	if code := send(`{"approved":true,"scope":"bash(*)"}`); code != http.StatusBadRequest {
		t.Fatalf("a wider scope = %d, want 400", code)
	}
	if code := send(`{"approved":true}`); code != http.StatusNoContent {
		t.Fatalf("approve once = %d, want 204", code)
	}
	if _, _, scope, _ := f.ApprovalResult(context.Background(), "ap-far"); scope != "" {
		t.Fatalf("approve once was recorded with scope %q", scope)
	}
}

// timedOutRow commits the row and then reports a failure, as a write whose
// client timed out after the database committed it would.
type timedOutRow struct{ *fakeApprovals }

func (r timedOutRow) AskApproval(_ context.Context, a store.Approval) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending[a.SessionID] = a
	return "", errors.New("timeout: context deadline exceeded")
}

// A row whose write reported failure may still exist; it is closed by the id
// the server chose for it.
func TestARowWhoseWriteFailedIsClosed(t *testing.T) {
	f := newFakeApprovals()
	l := &liveSession{ID: "s-lost", allowed: map[string]bool{}, durable: timedOutRow{f}}
	go answerWhenAsked(l, false, "")
	if _, err := l.Approve(context.Background(), "bash", []byte(`{}`), policy.Result{}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	f.mu.Lock()
	id := f.pending["s-lost"].ID
	f.mu.Unlock()
	if id == "" {
		t.Fatal("the server did not choose the row's id")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		ended := f.ended[id]
		f.mu.Unlock()
		if ended {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the row whose write failed was left open")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
