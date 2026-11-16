package server

import (
	"context"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// A row DenyPending answered is the system's even when the turn reads it
// from the store before the request is marked as the system's; a row a
// person answered stays the reviewer's.
func TestTheSystemsRowIsTheSystemsAnswer(t *testing.T) {
	for _, tc := range []struct {
		by   string
		want string
	}{{systemApprover, agent.BySystem}, {"local:alice", agent.ByReviewer}} {
		f := newFakeApprovals()
		live := &liveSession{allowed: map[string]string{}, durable: f}
		ctx, ans := agent.ExpectAnswer(context.Background())
		done := make(chan bool, 1)
		go func() {
			ok, _ := live.Approve(agent.WithRequestID(ctx, "req-1"), "write", nil, policy.Result{Decision: policy.Ask})
			done <- ok
		}()
		p := waitAsked(live, "req-1")
		<-p.ready
		live.mu.Lock()
		id := p.DurableID
		live.mu.Unlock()
		// Written to the row only: the in-memory request is not marked.
		if ok, _ := f.AnswerApproval(context.Background(), id, false, "", tc.by); !ok {
			t.Fatal("the row was not answered")
		}
		select {
		case ok := <-done:
			if ok {
				t.Fatalf("%s: a denial approved", tc.by)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the turn did not take the row's answer", tc.by)
		}
		if ans.By != tc.want {
			t.Errorf("row answered by %s: recorded by %q, want %q", tc.by, ans.By, tc.want)
		}
	}
}
