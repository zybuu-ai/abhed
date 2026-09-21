package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
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
