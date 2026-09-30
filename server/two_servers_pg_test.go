package server

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
)

// pgExec runs one statement on the test database, in the tenant the servers use.
func pgExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("ABHED_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', 'acme', false)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

// pgHolder is the session's holder as the database has it.
func pgHolder(t *testing.T, id string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("ABHED_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', 'acme', false)"); err != nil {
		t.Fatal(err)
	}
	var h *string
	if err := conn.QueryRow(ctx, "SELECT node_id FROM sessions WHERE id = $1", id).Scan(&h); err != nil {
		t.Fatal(err)
	}
	if h == nil {
		return ""
	}
	return *h
}

// openSharedPG opens one connection pool on the test database, as each server
// process of a deployment does; it skips without ABHED_TEST_DSN.
func openSharedPG(t *testing.T) *store.Postgres {
	t.Helper()
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run two servers on one Postgres")
	}
	cfg := store.DefaultConfig(dsn)
	cfg.Tenant, cfg.SingleRole = "acme", true
	p, err := store.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open (ABHED_TEST_DSN must be a plain role): %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// Two servers with no node id share one database. A child running on A is
// never reconciled by B, whichever message reaches B: B's instance sees A's
// heartbeat and leaves the session alone.
func TestTwoServersOnePostgresLiveChildNotReconciled(t *testing.T) {
	a := newBGServer(t, openSharedPG(t), "one")
	b := newBGServer(t, openSharedPG(t))
	id := a.start("bg:one", false)
	<-a.ended
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"x"}`); rec.Code != http.StatusConflict {
		t.Fatalf("B took a session whose child runs on A: %d %s", rec.Code, rec.Body)
	}
	if n := countType(b.events(id), agent.EvSubagentReturn); n != 0 {
		t.Fatalf("B reconciled A's live child (%d returns)", n)
	}
	a.ad.release("one")
	waitUntil(t, "A's closing end", func() bool { e, _ := agent.LastEnd(a.events(id)); return e.Settled })
}

// A holder whose heartbeats stalled past the stale window loses the session
// to another process, and then stops: its next heartbeat does not take the
// row back, its child is stopped, it drops the session, and it writes
// nothing more to the session's record.
func TestTwoServersOnePostgresFencesTheOldHolder(t *testing.T) {
	old := nodeHeartbeat
	nodeHeartbeat = 100 * time.Millisecond
	defer func() { nodeHeartbeat = old }()
	a := newBGServer(t, openSharedPG(t), "one")
	b := newBGServer(t, openSharedPG(t))
	id := a.start("bg:one", false)
	<-a.ended
	aLive := a.live(id)
	// A's heartbeats stall, as through a database failover.
	pgExec(t, "UPDATE sessions SET node_seen_at = now() - interval '10 minutes' WHERE id = $1", id)
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"x"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("B could not take the stale session: %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "A to let go", func() bool { return a.live(id) == nil && aLive.Loop.Background.Live() == 0 })
	time.Sleep(3 * nodeHeartbeat)
	if h := pgHolder(t, id); h == a.s.holder {
		t.Fatalf("A's heartbeat took the row back: holder %q", h)
	}
	for _, r := range payloadsOf(b.events(id), agent.EvSubagentReturn) {
		if r["reason"] == string(agent.TermLeaseLost) {
			t.Fatalf("A wrote into the session after losing it: %v", r)
		}
	}
	a.ad.release("one")
	waitUntil(t, "B's run", func() bool { return b.state(id) != "running" })
}
