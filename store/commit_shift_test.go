package store

import (
	"context"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A logical restore into a cluster whose transaction counter is behind the
// one that wrote the rows keeps their commit_tx. Without the shift, a reader
// resuming from its cursor would never see rows written after the restore.
// Here the old cluster's ids are stood in for by raising the rows' commit_tx
// as the owner, behind the append-only trigger; the next migrate must lift
// new rows above them.
func TestCommitOrderSurvivesARestoreIntoAnotherCluster(t *testing.T) {
	p := runtimeStore(t, "t-shift")
	ctx := context.Background()
	id := testID(t, "sess-shift-")
	newSession(t, p, id, "t-shift")
	for i := int64(1); i <= 3; i++ {
		if err := p.Append(ev(id, i, agent.EvAgentMessage, agent.Trusted, map[string]int64{"n": i})); err != nil {
			t.Fatal(err)
		}
	}
	owner := openStore(t, "t-shift")
	tx, err := owner.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{
		`ALTER TABLE events DISABLE TRIGGER events_no_update`,
		`UPDATE events SET commit_tx = (commit_tx::text::numeric + 5000000)::text::xid8 WHERE session_id = '` + id + `'`,
		`ALTER TABLE events ENABLE TRIGGER events_no_update`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := owner.pool.QueryRow(ctx, `SELECT shift::text FROM commit_shift`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// What a restore's `abhed migrate` does.
	if err := applySchema(ctx, owner.pool); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := owner.pool.QueryRow(ctx, `SELECT shift::text FROM commit_shift`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatalf("shift stayed %s with rows ahead of the transaction counter", before)
	}
	// Migrating again, with nothing ahead, leaves it.
	if err := applySchema(ctx, owner.pool); err != nil {
		t.Fatal(err)
	}
	var again string
	if err := owner.pool.QueryRow(ctx, `SELECT shift::text FROM commit_shift`).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if again != after {
		t.Fatalf("a second migrate moved the shift from %s to %s", after, again)
	}

	cursor, seen := scanAll(t, p, CommitPos{})
	if len(seen) != 3 {
		t.Fatalf("a reader sees %d of the 3 restored rows", len(seen))
	}
	if err := p.Append(ev(id, 4, agent.EvAgentMessage, agent.Trusted, map[string]int64{"n": 4})); err != nil {
		t.Fatal(err)
	}
	if _, next := scanAll(t, p, cursor); len(next) != 1 {
		t.Fatalf("a reader at its cursor sees %d rows written after the restore, want 1", len(next))
	}
	if r, err := p.VerifyChain(ctx, id); err != nil || !r.OK() {
		t.Fatalf("chain after the shift: %+v, %v", r, err)
	}

	// It is never lowered, and the runtime role cannot touch it.
	if _, err := owner.pool.Exec(ctx, `UPDATE commit_shift SET shift = 0`); err == nil {
		t.Fatal("the owner lowered the shift")
	}
	if _, err := owner.pool.Exec(ctx, `DELETE FROM commit_shift`); err == nil {
		t.Fatal("the owner deleted the shift")
	}
	if _, err := p.pool.Exec(ctx, `UPDATE commit_shift SET shift = shift + 1`); err == nil {
		t.Fatal("the runtime role changed the shift")
	}
}
