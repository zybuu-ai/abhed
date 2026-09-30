package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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

// An open row is taken over only when its holder's heartbeat is stale or was
// never written, and the claim writes the new holder, so it is exclusive: of
// many processes claiming at once, exactly one wins.
func TestClaimOrphan(t *testing.T) {
	p := openStore(t, "t-orphan")
	ctx := context.Background()
	id := testID(t, "sess-orph-")
	newSession(t, p, id, "t-orphan")
	if err := p.Append(ev(id, 1, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "go"})); err != nil {
		t.Fatal(err)
	}
	// A live holder, a single server's own instance id included: never taken.
	if err := p.ClaimNode(ctx, id, "instance-a"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.ClaimOrphan(ctx, id, "instance-b", 2*time.Minute); ok {
		t.Fatal("a session with a live holder was taken")
	}
	if _, err := p.ClaimOrphan(ctx, id, "", 2*time.Minute); err == nil {
		t.Fatal("a claim with no holder was accepted")
	}
	// Its holder stops heartbeating: many claim at once, one wins.
	if _, err := p.pool.Exec(ctx, `UPDATE sessions SET node_seen_at = now() - interval '10 minutes' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	const claimers = 8
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range claimers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := p.ClaimOrphan(ctx, id, fmt.Sprintf("instance-%d", i), 2*time.Minute)
			if err != nil {
				t.Error(err)
			}
			if ok {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := wins.Load(); n != 1 {
		t.Fatalf("%d claimers took the same orphan, want exactly one", n)
	}
	// The winner is now the live holder: a later claim fails too.
	if ok, _ := p.ClaimOrphan(ctx, id, "instance-z", 2*time.Minute); ok {
		t.Fatal("an orphan was taken again from its new holder")
	}
	// A row whose holder never wrote a heartbeat is an orphan.
	if _, err := p.pool.Exec(ctx, `UPDATE sessions SET node_id = NULL, node_seen_at = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.ClaimOrphan(ctx, id, "instance-y", 2*time.Minute); !ok {
		t.Fatal("an open row with no holder was not taken")
	}
	// An ended row is not an orphan.
	if err := p.Append(ev(id, 2, agent.EvSessionEnded, agent.Trusted, agent.SessionEnded{Reason: agent.TermShutdown})); err != nil {
		t.Fatal(err)
	}
	if _, err := p.pool.Exec(ctx, `UPDATE sessions SET node_id = NULL, node_seen_at = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.ClaimOrphan(ctx, id, "instance-x", 2*time.Minute); ok {
		t.Fatal("an ended session was taken as an orphan")
	}
}

// A displaced holder cannot take the session back: its claim is refused
// while the new holder is live, and its fenced heartbeat reports the loss.
// A holder that went stale can be replaced.
func TestClaimNodeNeverTakesALiveHolder(t *testing.T) {
	p := openStore(t, "t-fence")
	ctx := context.Background()
	id := testID(t, "sess-fence-")
	newSession(t, p, id, "t-fence")
	if err := p.ClaimNode(ctx, id, "instance-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.pool.Exec(ctx, `UPDATE sessions SET node_seen_at = now() - interval '10 minutes' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.ClaimOrphan(ctx, id, "instance-c", HolderStale); !ok {
		t.Fatal("the stale holder's session was not taken")
	}
	if err := p.ClaimNode(ctx, id, "instance-a"); !errors.Is(err, ErrHeldElsewhere) {
		t.Fatalf("the displaced holder's claim: %v", err)
	}
	if held, err := p.RenewNode(ctx, id, "instance-a"); err != nil || held {
		t.Fatalf("the displaced holder's heartbeat: held %v, %v", held, err)
	}
	if held, err := p.RenewNode(ctx, id, "instance-c"); err != nil || !held {
		t.Fatalf("the holder's heartbeat: held %v, %v", held, err)
	}
	var holder string
	if err := p.pool.QueryRow(ctx, `SELECT node_id FROM sessions WHERE id = $1`, id).Scan(&holder); err != nil || holder != "instance-c" {
		t.Fatalf("holder %q, %v", holder, err)
	}
	if err := p.ClaimNode(ctx, id, "instance-c"); err != nil {
		t.Fatalf("the holder claiming again: %v", err)
	}
}
