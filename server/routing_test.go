package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/store"
)

// fakeRouter records what the server asked it, so the decision logic can be
// tested without a database.
type fakeRouter struct {
	EventStore
	node      string
	err       error
	claimed   []string
	released  []string
	lookedFor []string
}

func (f *fakeRouter) ClaimNode(_ context.Context, sessionID, nodeID string) error {
	f.claimed = append(f.claimed, sessionID+"@"+nodeID)
	return f.err
}

func (f *fakeRouter) ReleaseNode(_ context.Context, sessionID, nodeID string) error {
	f.released = append(f.released, sessionID+"@"+nodeID)
	return f.err
}

func (f *fakeRouter) NodeFor(_ context.Context, sessionID string, _ time.Duration) (string, error) {
	f.lookedFor = append(f.lookedFor, sessionID)
	return f.node, f.err
}

// Routing is off unless the deployment says which node this is. A single
// server must behave exactly as it did before this existed.
func TestRoutingIsOffWithoutANodeID(t *testing.T) {
	s := &Server{store: &fakeRouter{}, opts: Options{}}
	if _, ok := s.router(); ok {
		t.Fatal("routing engaged with no NodeID; a single-node deployment would start claiming")
	}
	if got := s.elsewhere(context.Background(), "s-1"); got != "" {
		t.Fatalf("elsewhere = %q with routing off, want empty", got)
	}
}

// A store that cannot record a claim leaves routing off rather than failing.
func TestRoutingIsOffWithoutARoutingStore(t *testing.T) {
	s := &Server{store: memoryOnly{}, opts: Options{NodeID: "node-a"}}
	if _, ok := s.router(); ok {
		t.Fatal("routing engaged with a store that cannot record a claim")
	}
}

// The node holding a session is only worth reporting when it is not this one.
func TestElsewhereIgnoresOurselves(t *testing.T) {
	f := &fakeRouter{node: "node-a"}
	s := &Server{store: f, opts: Options{NodeID: "node-a"}}
	if got := s.elsewhere(context.Background(), "s-1"); got != "" {
		t.Fatalf("elsewhere = %q for our own node, want empty so the ordinary 404 answers", got)
	}
}

func TestElsewhereReportsAnotherNode(t *testing.T) {
	f := &fakeRouter{node: "node-b"}
	s := &Server{store: f, opts: Options{NodeID: "node-a"}}
	if got := s.elsewhere(context.Background(), "s-1"); got != "node-b" {
		t.Fatalf("elsewhere = %q, want node-b", got)
	}
	if len(f.lookedFor) != 1 || f.lookedFor[0] != "s-1" {
		t.Fatalf("looked up %v, want [s-1]", f.lookedFor)
	}
}

// A lookup failure must not turn into a redirect to nowhere: the caller gets
// the ordinary not-found answer instead.
func TestElsewhereIsSilentOnError(t *testing.T) {
	f := &fakeRouter{node: "node-b", err: errors.New("database is down")}
	s := &Server{store: f, opts: Options{NodeID: "node-a"}}
	if got := s.elsewhere(context.Background(), "s-1"); got != "" {
		t.Fatalf("elsewhere = %q despite a lookup error, want empty", got)
	}
}

// An unheld session is served here rather than redirected.
func TestElsewhereEmptyWhenNoNodeHoldsIt(t *testing.T) {
	s := &Server{store: &fakeRouter{node: ""}, opts: Options{NodeID: "node-a"}}
	if got := s.elsewhere(context.Background(), "s-1"); got != "" {
		t.Fatalf("elsewhere = %q for an unheld session, want empty", got)
	}
}

// A claim that cannot be written is logged and ignored — refusing to start a
// turn because bookkeeping failed is worse than a misrouted request.
func TestClaimFailureDoesNotStopTheTurn(t *testing.T) {
	f := &fakeRouter{err: errors.New("write failed")}
	s := &Server{store: f, opts: Options{NodeID: "node-a"}, log: discardLogger()}
	s.claimNode(context.Background(), "s-1") // must not panic
	s.releaseNode("s-1")                     // must not panic
	if len(f.claimed) != 1 || len(f.released) != 1 {
		t.Fatalf("claimed %v released %v, want one of each attempted", f.claimed, f.released)
	}
}

// memoryOnly is a store with no routing support.
type memoryOnly struct{ EventStore }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeApprovals stands in for the durable store.
type fakeApprovals struct {
	EventStore
	mu        sync.Mutex
	pending   map[string]store.Approval
	answered  map[string]bool
	askErr    error
	answerErr error
	asked     int
}

func (f *fakeApprovals) askedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked
}

func newFakeApprovals() *fakeApprovals {
	return &fakeApprovals{pending: map[string]store.Approval{}, answered: map[string]bool{}}
}

func (f *fakeApprovals) AskApproval(_ context.Context, a store.Approval) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.askErr != nil {
		return "", f.askErr
	}
	f.asked++
	a.ID = "ap-test"
	f.pending[a.SessionID] = a
	return a.ID, nil
}

func (f *fakeApprovals) AnswerApproval(_ context.Context, id string, approved bool, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.answerErr != nil {
		return false, f.answerErr
	}
	if _, already := f.answered[id]; already {
		return false, nil
	}
	f.answered[id] = approved
	return true, nil
}

func (f *fakeApprovals) ApprovalResult(_ context.Context, id string) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.answered[id]
	return a, ok, nil
}

func (f *fakeApprovals) PendingApproval(_ context.Context, sessionID string) (store.Approval, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.pending[sessionID]
	return a, ok, nil
}

// A store that cannot hold approvals leaves the in-memory path alone, which
// is correct for a single node.
func TestApprovalStoreIsOptional(t *testing.T) {
	s := &Server{store: memoryOnly{}}
	if s.approvalStore() != nil {
		t.Fatal("a store that cannot hold approvals was treated as if it could")
	}
}

func TestApprovalStoreDetected(t *testing.T) {
	s := &Server{store: newFakeApprovals()}
	if s.approvalStore() == nil {
		t.Fatal("a store that can hold approvals was not detected")
	}
}

// The durable record is what lets an answer arrive anywhere. Recording it
// must not be skipped when the store supports it.
func TestApprovalIsRecordedDurably(t *testing.T) {
	f := newFakeApprovals()
	l := &liveSession{
		ID:      "s-1",
		allowed: map[string]bool{}, durable: f,
	}
	// Answer immediately through the durable path so Approve returns.
	go func() {
		for f.askedCount() == 0 {
			time.Sleep(time.Millisecond)
		}
		_, _ = f.AnswerApproval(context.Background(), "ap-test", true, "reviewer")
	}()

	ok, err := l.Approve(context.Background(), "bash",
		[]byte(`{"command":"ls"}`), policy.Result{Reason: "mutating"})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if !ok {
		t.Fatal("an approval answered through the store was not honoured")
	}
	if f.askedCount() != 1 {
		t.Fatalf("asked %d times, want 1 durable record", f.askedCount())
	}
}

// A store that cannot record the request must not block the turn: the
// in-memory answer still reaches the turn on a single node.
func TestApproveStillWorksWhenRecordingFails(t *testing.T) {
	f := newFakeApprovals()
	f.askErr = errors.New("database is down")
	l := &liveSession{
		ID:      "s-2",
		allowed: map[string]bool{}, durable: f,
	}
	go answerWhenAsked(l, true, "")

	ok, err := l.Approve(context.Background(), "bash", []byte(`{}`), policy.Result{})
	if err != nil || !ok {
		t.Fatalf("Approve = %v, %v — a failed durable write must not lose the answer", ok, err)
	}
}

// An "always allow" scope is remembered whichever path answered.
func TestScopeIsRememberedFromTheDurablePath(t *testing.T) {
	f := newFakeApprovals()
	l := &liveSession{
		ID:      "s-3",
		allowed: map[string]bool{}, durable: f,
	}
	go func() {
		for f.askedCount() == 0 {
			time.Sleep(time.Millisecond)
		}
		_, _ = f.AnswerApproval(context.Background(), "ap-test", true, "reviewer")
	}()

	if _, err := l.Approve(context.Background(), "bash", []byte(`{}`),
		policy.Result{Scope: "bash(go test*)"}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	l.mu.Lock()
	remembered := l.allowed["bash(go test*)"]
	l.mu.Unlock()
	if !remembered {
		t.Fatal("the scope was not remembered, so the next matching call re-prompts")
	}
}

// countingRouter records how often a session's claim is refreshed.
type countingRouter struct {
	EventStore
	mu     sync.Mutex
	claims int
}

func (c *countingRouter) ClaimNode(context.Context, string, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claims++
	return nil
}
func (c *countingRouter) ReleaseNode(context.Context, string, string) error { return nil }
func (c *countingRouter) NodeFor(context.Context, string, time.Duration) (string, error) {
	return "", nil
}
func (c *countingRouter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claims
}

// A claim that is never refreshed ages out of nodeStale while the node is
// still healthy, and the session stops being routable. The heartbeat is what
// keeps a long turn reachable.
func TestHeartbeatRefreshesTheClaim(t *testing.T) {
	r := &countingRouter{}
	s := &Server{
		store:   r,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		running: map[string]*liveSession{},
		opts:    Options{NodeID: "node-a"},
	}

	// A tick faster than the real 30s keeps the test quick; the mechanism is
	// identical.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := s.heartbeatNodeEvery(ctx, "s-beat", 20*time.Millisecond)
	defer stop()

	// Wait for refreshes rather than for a duration: under -race the build is
	// slow enough that a fixed window makes this flaky.
	deadline := time.After(5 * time.Second)
	for r.count() < 3 {
		select {
		case <-deadline:
			t.Fatalf("claim refreshed only %d times — a long turn would go stale", r.count())
		case <-time.After(5 * time.Millisecond):
		}
	}

	stop()
	// One tick may already be in flight when stop lands; what matters is that
	// the beating ends, not the exact count at the instant of stopping.
	time.Sleep(80 * time.Millisecond)
	settled := r.count()
	time.Sleep(120 * time.Millisecond)
	if after := r.count(); after != settled {
		t.Fatalf("heartbeat kept beating after stop: %d -> %d", settled, after)
	}
}

// Stopping twice must not panic: the run goroutine stops it, and the deferred
// stop runs too.
func TestHeartbeatStopIsIdempotent(t *testing.T) {
	s := &Server{
		store:   &countingRouter{},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		running: map[string]*liveSession{},
		opts:    Options{NodeID: "node-a"},
	}
	stop := s.heartbeatNodeEvery(context.Background(), "s-1", time.Hour)
	stop()
	stop()
}

// Without a router there is nothing to refresh, and the heartbeat must not
// start a goroutine that calls into a nil store.
func TestHeartbeatIsInertWithoutARouter(t *testing.T) {
	s := &Server{
		store:   memoryOnly{},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		running: map[string]*liveSession{},
	}
	stop := s.heartbeatNodeEvery(context.Background(), "s-1", time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	stop()
}
