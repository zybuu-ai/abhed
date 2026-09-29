package store

import (
	"context"
	"testing"
	"time"

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

// An open row no live node holds is taken over once; a fresh claim by
// another node, or activity since this single server started, keeps it.
func TestClaimOrphan(t *testing.T) {
	p := openStore(t, "t-orphan")
	ctx := context.Background()
	id := testID(t, "sess-orph-")
	newSession(t, p, id, "t-orphan")
	if err := p.Append(ev(id, 1, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "go"})); err != nil {
		t.Fatal(err)
	}
	// A single server that started before the last event: still active.
	if ok, _ := p.ClaimOrphan(ctx, id, "", 2*time.Minute, time.Now().Add(-time.Hour)); ok {
		t.Fatal("a session active since this server started was taken")
	}
	// Another node holds it freshly.
	if err := p.ClaimNode(ctx, id, "node-b"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.ClaimOrphan(ctx, id, "node-a", 2*time.Minute, time.Now()); ok {
		t.Fatal("a session another node holds was taken")
	}
	// Its claim goes stale: one node takes it, once.
	if _, err := p.pool.Exec(ctx, `UPDATE sessions SET node_seen_at = now() - interval '10 minutes' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	first, err := p.ClaimOrphan(ctx, id, "node-a", 2*time.Minute, time.Now())
	if err != nil || !first {
		t.Fatalf("first: %v %v", first, err)
	}
	if second, _ := p.ClaimOrphan(ctx, id, "node-c", 2*time.Minute, time.Now()); second {
		t.Fatal("two nodes took the same orphan")
	}
	// An ended row is not an orphan.
	if err := p.Append(ev(id, 2, agent.EvSessionEnded, agent.Trusted, agent.SessionEnded{Reason: agent.TermShutdown})); err != nil {
		t.Fatal(err)
	}
	if _, err := p.pool.Exec(ctx, `UPDATE sessions SET node_id = NULL, node_seen_at = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.ClaimOrphan(ctx, id, "", 2*time.Minute, time.Now().Add(time.Hour)); ok {
		t.Fatal("an ended session was taken as an orphan")
	}
}
