package store

import (
	"context"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A run's end with background children still running keeps the row open, so
// no other node claims the session while they run here; the closing end
// releases it with the totals.
func TestRowStaysOpenWhileChildrenRun(t *testing.T) {
	p := openStore(t, "t-bg")
	ctx := context.Background()
	id := testID(t, "sess-bg-")
	newSession(t, p, id, "t-bg")
	if err := p.Append(ev(id, 1, agent.EvSessionEnded, agent.Trusted,
		agent.SessionEnded{Reason: agent.TermCompleted, Turns: 2, Background: 1})); err != nil {
		t.Fatal(err)
	}
	rec, err := p.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.EndedAt != nil || rec.Turns != 2 {
		t.Fatalf("an end with children running: ended_at %v, turns %d", rec.EndedAt, rec.Turns)
	}
	if ok, _ := p.ClaimResume(ctx, id); ok {
		t.Fatal("another node claimed a session whose children run elsewhere")
	}
	if err := p.Append(ev(id, 2, agent.EvSessionEnded, agent.Trusted,
		agent.SessionEnded{Reason: agent.TermCompleted, Turns: 2, Settled: true})); err != nil {
		t.Fatal(err)
	}
	if rec, _ = p.GetSession(ctx, id); rec.EndedAt == nil {
		t.Fatal("the closing end did not release the row")
	}
	if ok, _ := p.ClaimResume(ctx, id); !ok {
		t.Fatal("a settled session could not be claimed")
	}
}
