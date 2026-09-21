package store

import (
	"context"
	"errors"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A deleted session must be gone from every read path — list, get, events —
// while its rows stay in the table, because the events trigger forbids DELETE
// and that guarantee is the point of this store. Both halves are checked: a
// delete that hides nothing is a privacy bug, and one that removed rows would
// mean the append-only trigger is not doing its job.
func TestDeleteSessionHidesEverywhereButKeepsRows(t *testing.T) {
	p := openStore(t, "t-del")
	ctx := context.Background()
	id := testID(t, "sess-del-")
	newSession(t, p, id, "t-del")
	if err := p.Append(ev(id, 1, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "secret"})); err != nil {
		t.Fatalf("append: %v", err)
	}
	if evs, _ := p.Events(id); len(evs) != 1 {
		t.Fatalf("precondition: expected 1 event, got %d", len(evs))
	}

	if err := p.DeleteSession(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := p.DeleteSession(id); err != nil {
		t.Fatalf("second delete should be a no-op, got %v", err)
	}

	if evs, err := p.Events(id); err != nil || len(evs) != 0 {
		t.Errorf("events still readable after delete: %d events, err=%v", len(evs), err)
	}
	if _, err := p.GetSession(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSession after delete: want ErrNotFound, got %v", err)
	}
	recs, err := p.ListSessions(ctx, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range recs {
		if r.ID == id {
			t.Errorf("deleted session still listed")
		}
	}

	// The rows are still there for the audit: the marking is the record of
	// the delete, not a way around the append-only log.
	var events int
	var deletedBy string
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1`, id).Scan(&events); err != nil {
		t.Fatalf("count: %v", err)
	}
	if err := p.pool.QueryRow(ctx, `SELECT deleted_by FROM sessions WHERE id = $1 AND deleted_at IS NOT NULL`, id).Scan(&deletedBy); err != nil {
		t.Fatalf("tombstone missing: %v", err)
	}
	if events != 1 || deletedBy != "tester" {
		t.Errorf("audit rows: events=%d deleted_by=%q, want 1 and \"tester\"", events, deletedBy)
	}
}

// Continuing a finished session must be claimed by exactly one process. The
// claim is a conditional update on the row, so two replicas racing for the
// same session cannot both win, and a deleted or still-running session
// cannot be claimed at all.
func TestClaimResumeIsExclusive(t *testing.T) {
	p := openStore(t, "t-claim")
	ctx := context.Background()
	id := testID(t, "sess-claim-")
	newSession(t, p, id, "t-claim")

	if ok, _ := p.ClaimResume(ctx, id); ok {
		t.Fatal("a session that never ended was claimable")
	}
	if _, err := p.pool.Exec(ctx, `UPDATE sessions SET ended_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	first, err := p.ClaimResume(ctx, id)
	if err != nil || !first {
		t.Fatalf("first claim: ok=%v err=%v", first, err)
	}
	if second, _ := p.ClaimResume(ctx, id); second {
		t.Fatal("a second claim succeeded while the first still held the session")
	}
	if _, err := p.pool.Exec(ctx, `UPDATE sessions SET ended_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteSession(id); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.ClaimResume(ctx, id); ok {
		t.Fatal("a deleted session was claimable")
	}
}
