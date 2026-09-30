package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// pendingOutsideARun puts an ask on a session whose run has ended, as a
// subagent still working after its parent's run would, and returns the
// cancel that ends the ask.
func pendingOutsideARun(t *testing.T, q *queueRig) (*liveSession, context.CancelFunc, chan struct{}) {
	t.Helper()
	close(q.adapter.gate)
	q.waitEnded()
	q.s.mu.Lock()
	live := q.s.running[q.id]
	q.s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = live.Approve(agent.WithSubagent(ctx, "child"), "bash", json.RawMessage(`{"command":"touch x"}`),
			policy.Result{Decision: policy.Ask, Reason: "a rule asks"})
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		live.mu.Lock()
		waiting := live.pending != nil && live.State == "waiting_approval"
		live.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the ask never became pending")
		}
	}
	return live, cancel, done
}

// An ask that ends after the run it came from leaves the session as it was,
// not running: the defer once forced "running" and revived a done session.
func TestApproveRestoresPriorState(t *testing.T) {
	q := newQueueRig(t)
	live, cancel, done := pendingOutsideARun(t, q)
	cancel()
	<-done
	live.mu.Lock()
	state := live.State
	live.mu.Unlock()
	if state != "done" {
		t.Fatalf("an ask outside a run left the session %q", state)
	}
}

// A message sent while only an ask is pending, and no run is live, starts a
// run; it was queued as steering into a loop that was not running.
func TestMessageWhileOnlyChildAskPendingStartsRun(t *testing.T) {
	q := newQueueRig(t)
	_, cancel, done := pendingOutsideARun(t, q)
	defer func() { cancel(); <-done }()
	rec := q.do("alice", "POST", "/messages", `{"prompt":"next"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("message = %d %s", rec.Code, rec.Body)
	}
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["delivery"] == "steered" {
		t.Fatalf("the message was queued into a loop that is not running: %v", out)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		msgs := q.userMessages()
		if len(msgs) == 2 && msgs[1].Text == "next" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the message never ran: %+v", q.userMessages())
		}
	}
}
