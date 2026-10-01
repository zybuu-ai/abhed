package server

import (
	"context"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zybuu-ai/abhed/config"
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
	// A's heartbeats stall, as through a database failover. A beat of A's may
	// land between the stall and B's claim, so B tries until it takes over.
	taken := false
	for range 20 {
		pgExec(t, "UPDATE sessions SET node_seen_at = now() - interval '10 minutes' WHERE id = $1", id)
		if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"x"}`); rec.Code == http.StatusAccepted {
			taken = true
			break
		}
	}
	if !taken {
		t.Fatal("B could not take the stale session")
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

// A session an older release is running has no holder. Another server's
// sweep leaves it alone while its record is still being written.
func TestTwoServersOnePostgresSweepSparesAFreshNoHolderSession(t *testing.T) {
	a := newBGServer(t, openSharedPG(t), "one")
	id := a.start("bg:one", false)
	<-a.ended
	pgExec(t, "UPDATE sessions SET node_id = NULL, node_seen_at = NULL WHERE id = $1", id)
	b := newBGServer(t, openSharedPG(t))
	b.s.RecoverOrphans(context.Background())
	if n := countType(b.events(id), agent.EvSubagentReturn); n != 0 {
		t.Fatalf("the sweep reconciled a running session with no holder (%d returns)", n)
	}
	a.ad.release("one")
	waitUntil(t, "A's closing end", func() bool { e, _ := agent.LastEnd(a.events(id)); return e.Settled })
}

// A holder that lost the session and has not heard yet (no heartbeat since)
// writes nothing into the record: the store refuses each append on the
// lease, takes no seq, and the refusal fences the session there.
func TestTwoServersOnePostgresStaleWritesAreRefused(t *testing.T) {
	old := nodeHeartbeat
	nodeHeartbeat = time.Hour // A is paused: no heartbeat before it writes
	defer func() { nodeHeartbeat = old }()
	a := newBGServer(t, openSharedPG(t), "one")
	b := newBGServer(t, openSharedPG(t))
	id := a.start("bg:one", false)
	<-a.ended
	al := a.live(id)
	pgExec(t, "UPDATE sessions SET node_seen_at = now() - interval '10 minutes' WHERE id = $1", id)
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"x"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("takeover: %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "B's run", func() bool { l := b.live(id); return l != nil && b.state(id) != "running" })
	child := payloadsOf(b.events(id), agent.EvSubagentSpawned)[0]["session"].(string)
	childBefore, _ := b.store.Events(child)
	before := len(b.events(id))
	for i := range 40 {
		if _, err := al.Loop.Recorder.Record(agent.EvAgentDelta, agent.ActorAgent, agent.Untrusted, map[string]any{"text": "stale", "n": i}); err == nil {
			t.Fatalf("the old holder's write %d landed in the new holder's record", i)
		}
	}
	if after := len(b.events(id)); after != before {
		t.Fatalf("the record grew from %d to %d events", before, after)
	}
	waitUntil(t, "A fenced", func() bool { return a.live(id) == nil })
	// B goes on, its sequence intact.
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"again"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("B's next message: %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "B's second run", func() bool { return b.state(id) != "running" })
	if e, _ := agent.LastEnd(b.events(id)); e.Reason != agent.TermCompleted {
		t.Fatalf("B's run ended %s", e.Reason)
	}
	// A's child, stopped by the fence, writes nothing into its record either.
	a.ad.release("one")
	time.Sleep(200 * time.Millisecond)
	if childAfter, _ := b.store.Events(child); len(childAfter) != len(childBefore) {
		t.Fatalf("the old holder's child wrote %d events after the takeover", len(childAfter)-len(childBefore))
	}
}

// A session let go after its run is claimed back by the next message on the
// same server, and runs as before.
func TestOnePostgresSessionContinuesAfterItsRun(t *testing.T) {
	a := newBGServer(t, openSharedPG(t))
	id := a.start("hello", false)
	<-a.ended
	waitUntil(t, "let go", func() bool { return a.live(id).unclaimed.Load() })
	for i := range 2 {
		if rec := a.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"again"}`); rec.Code != http.StatusAccepted {
			t.Fatalf("message %d: %d %s", i, rec.Code, rec.Body)
		}
		waitUntil(t, "the run", func() bool { return a.state(id) != "running" })
	}
	if n := countType(a.events(id), agent.EvUserMessage); n != 3 {
		t.Fatalf("%d messages recorded, want 3", n)
	}
}

// startSweeper runs the process's periodic sweep the moment a session's first
// event is written, before the server has it in its running map.
type startSweeper struct {
	*store.Postgres
	srv   atomic.Pointer[Server]
	swept atomic.Int32
}

func (h *startSweeper) Append(ev agent.Event) error {
	if err := h.Postgres.Append(ev); err != nil {
		return err
	}
	if ev.Type == agent.EvSessionStarted && ev.ParentID == "" {
		if s := h.srv.Load(); s != nil {
			h.swept.Add(int32(s.RecoverOrphans(context.Background()))) // #nosec G115 -- a test count
		}
	}
	return nil
}

// On Postgres too, a process's own sweep never reconciles a session it is
// starting, whose row is held under its own id.
func TestOnePostgresSweepSparesASessionItIsStarting(t *testing.T) {
	h := &startSweeper{Postgres: openSharedPG(t)}
	a := newBGServerWith(t, h, func(_ *config.Config, o *Options) { o.NodeID = "node-" + newSessionID() })
	h.srv.Store(a.s)
	id := a.start("hello", false)
	<-a.ended
	for _, e := range payloadsOf(a.events(id), agent.EvSessionEnded) {
		if e["recovered"] == true {
			t.Fatal("the process's own sweep reconciled a session it was starting")
		}
	}
}

// With an event tap (telemetry), the server still sees the store's
// liveness, fencing and let-go: every message to a session runs and is
// recorded, not only the first.
func TestOnePostgresWithATapEveryMessageRuns(t *testing.T) {
	var seen atomic.Int32
	a := newBGServerWith(t, openSharedPG(t), func(_ *config.Config, o *Options) { o.EventTap = func(agent.Event) { seen.Add(1) } })
	id := a.start("hello", false)
	<-a.ended
	for i := range 3 {
		waitUntil(t, "the run", func() bool { return a.state(id) != "running" })
		if rec := a.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"again"}`); rec.Code != http.StatusAccepted {
			t.Fatalf("message %d: %d %s", i, rec.Code, rec.Body)
		}
		waitUntil(t, "the message recorded", func() bool { return countType(a.events(id), agent.EvUserMessage) == i+2 })
	}
	if l := a.live(id); l == nil || l.fenced.Load() || seen.Load() == 0 {
		t.Fatalf("the session was fenced, or the tap saw nothing (%d)", seen.Load())
	}
	// Let go after its run, as without a tap.
	waitUntil(t, "the session let go", func() bool { return a.live(id).unclaimed.Load() })

	// A session with a task running holds its row under this process's id.
	b := newBGServerWith(t, openSharedPG(t), func(_ *config.Config, o *Options) { o.EventTap = func(agent.Event) {} }, "one")
	bid := b.start("bg:one", false)
	<-b.ended
	if h := pgHolder(t, bid); h != b.s.holder {
		t.Fatalf("behind a tap the holder is %q, want %q", h, b.s.holder)
	}
	b.ad.release("one")
}

// A workbench hold on a session this process created runs from the last
// manual write, not the first: a write late in the hold extends it.
func TestOnePostgresWorkbenchHoldRunsFromTheLastWrite(t *testing.T) {
	hold := manualHold
	manualHold = 1200 * time.Millisecond
	t.Cleanup(func() { manualHold = hold })
	a := newBGServer(t, openSharedPG(t))
	id := a.start("hello", false)
	<-a.ended
	live := a.live(id)
	waitUntil(t, "let go", func() bool { return live.unclaimed.Load() })
	ends := func() int { return countType(a.events(id), agent.EvSessionEnded) }
	before := ends()
	write := func() {
		t.Helper()
		if _, err := live.Loop.Recorder.Record(agent.EvChangeAccepted, agent.ActorUser, agent.Trusted, map[string]any{"path": "a"}); err != nil {
			t.Fatal(err)
		}
	}
	write() // claims the session and starts the hold
	if live.unclaimed.Load() {
		t.Fatal("a workbench write did not claim the session")
	}
	time.Sleep(manualHold * 3 / 4) // t+90s of a two-minute hold
	write()
	time.Sleep(manualHold / 2) // past the first write's hold, inside the second's
	if n := ends(); n != before {
		t.Fatalf("the hold was released %v after the first write, before the last one's ran out", manualHold*5/4)
	}
	waitUntil(t, "the release", func() bool { return ends() == before+1 && live.unclaimed.Load() })
}
