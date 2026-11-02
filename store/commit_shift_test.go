package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// shiftOf reads the database's commit_shift.
func shiftOf(t *testing.T, p *Postgres) string {
	t.Helper()
	var s string
	if err := p.pool.QueryRow(context.Background(), `SELECT shift::text FROM public.commit_shift`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A logical restore into a cluster whose transaction counter is behind the
// one that wrote the rows keeps their commit_tx. Without the shift, a reader
// resuming from its cursor would never see rows written after the restore.
// Here the old cluster's ids are stood in for by raising the rows' commit_tx
// as the owner, behind the append-only trigger; the next migrate must lift
// new rows above them. Each run has a tenant of its own, so the test can run
// again on the same database; the shift it leaves there only ever rises.
func TestCommitOrderSurvivesARestoreIntoAnotherCluster(t *testing.T) {
	tenant := testID(t, "t-shift-")
	p := runtimeStore(t, tenant)
	ctx := context.Background()
	id := testID(t, "sess-shift-")
	newSession(t, p, id, tenant)
	for i := int64(1); i <= 3; i++ {
		if err := p.Append(ev(id, i, agent.EvAgentMessage, agent.Trusted, map[string]int64{"n": i})); err != nil {
			t.Fatal(err)
		}
	}
	owner := openStore(t, tenant)
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

	// A runtime server started on the restored database before a migrate
	// would sort new events below exported ones: it refuses.
	cfg := DefaultConfig(os.Getenv("ABHED_TEST_RUNTIME_DSN"))
	cfg.Tenant = tenant
	if q, err := Open(ctx, cfg); err == nil {
		q.Close()
		t.Fatal("a runtime server started with the record's commit order ahead of the cluster")
	} else if !errors.Is(err, errCommitOrderAhead) || !strings.Contains(err.Error(), "abhed migrate") {
		t.Fatalf("open before the migrate: %v; want the commit-order refusal naming abhed migrate", err)
	}

	before := shiftOf(t, owner)
	// What a restore's `abhed migrate` does.
	if err := applySchema(ctx, owner.pool); err != nil {
		t.Fatal(err)
	}
	after := shiftOf(t, owner)
	if after == before {
		t.Fatalf("shift stayed %s with rows ahead of the transaction counter", before)
	}
	// Migrating again, with nothing ahead, leaves it.
	if err := applySchema(ctx, owner.pool); err != nil {
		t.Fatal(err)
	}
	if again := shiftOf(t, owner); again != after {
		t.Fatalf("a second migrate moved the shift from %s to %s", after, again)
	}
	// And now the runtime server starts.
	q, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open after the migrate: %v", err)
	}
	q.Close()

	cursor, seen := scanAll(t, p, CommitPos{})
	if len(seen) != 3 {
		t.Fatalf("a reader sees %d of the 3 restored rows", len(seen))
	}
	end, err := p.CommitEnd(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if end != cursor {
		t.Fatalf("CommitEnd %v after the shift, want the reader's position %v", end, cursor)
	}
	if err := p.Append(ev(id, 4, agent.EvAgentMessage, agent.Trusted, map[string]int64{"n": 4})); err != nil {
		t.Fatal(err)
	}
	next, nextIDs := scanAll(t, p, cursor)
	if len(nextIDs) != 1 {
		t.Fatalf("a reader at its cursor sees %d rows written after the restore, want 1", len(nextIDs))
	}
	if end, err = p.CommitEnd(ctx); err != nil || end != next || end.Tx <= cursor.Tx {
		t.Fatalf("CommitEnd %v (%v) after a new row, want %v, past %v", end, err, next, cursor)
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

// The runtime role may make temporary tables. One named commit_shift must not
// let it choose its rows' commit_tx (and so hide them from every
// commit-ordered reader), and one named sessions must not get a row past the
// refusal of a deleted session. The trigger and its functions name the real
// tables and search pg_temp last.
func TestTemporaryTablesDoNotStandInForTheRecordsOwn(t *testing.T) {
	tenant := testID(t, "t-temp-")
	p := runtimeStore(t, tenant)
	ctx := context.Background()
	live, gone := testID(t, "sess-temp-live-"), testID(t, "sess-temp-gone-")
	newSession(t, p, live, tenant)
	newSession(t, p, gone, tenant)
	if err := p.Append(ev(gone, 1, agent.EvAgentMessage, agent.Trusted, map[string]int{"n": 1})); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteSession(gone); err != nil {
		t.Fatal(err)
	}
	// The runtime role's own connection and its own SQL, as someone holding
	// the server's credentials would write it.
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, q := range []string{
		`CREATE TEMP TABLE commit_shift AS SELECT (10 - pg_current_xact_id()::text::numeric) AS shift, true AS one`,
		`CREATE TEMP TABLE sessions (id text, deleted_at timestamptz)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	defer func() { _, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS pg_temp.commit_shift, pg_temp.sessions`) }()
	insert := `INSERT INTO public.events (id, session_id, tenant_id, seq, type, payload, actor, trust)
		VALUES ($1, $2, $3, $4, 'agent.message', '{}', 'agent', 'trusted') RETURNING commit_tx::text`
	// Every transaction from here takes an id at or past xmax, so with the
	// real shift its commit_tx is at least this.
	var floor, tx string
	if err := conn.QueryRow(ctx, `SELECT (pg_snapshot_xmax(pg_current_snapshot())::text::numeric + shift)::text
		FROM public.commit_shift`).Scan(&floor); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, insert, live+"-ev-1", live, tenant, 1).Scan(&tx); err != nil {
		t.Fatal(err)
	}
	var low bool
	if err := conn.QueryRow(ctx, `SELECT $1::numeric < $2::numeric`, tx, floor).Scan(&low); err != nil {
		t.Fatal(err)
	}
	if low {
		t.Fatalf("a temporary commit_shift set the row's commit_tx to %s, below %s", tx, floor)
	}
	if _, err := conn.Exec(ctx, insert, gone+"-ev-2", gone, tenant, 2); err == nil {
		t.Fatal("a temporary sessions table let a row into a deleted session")
	}
	if _, ids := scanAll(t, p, CommitPos{}); len(ids) != 2 {
		t.Fatalf("a commit-ordered reader sees %d of the tenant's 2 rows", len(ids))
	}
}
