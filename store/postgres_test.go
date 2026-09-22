package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Integration tests run against a real Postgres. Set ABHED_TEST_DSN to enable;
// they are skipped otherwise so `go test ./...` works without a database.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run store integration tests")
	}
	return dsn
}

func openStore(t *testing.T, tenant string) *Postgres {
	t.Helper()
	cfg := singleRoleConfig(testDSN(t))
	cfg.Tenant = tenant
	p, err := Open(context.Background(), cfg)
	if err != nil {
		// Open refuses a superuser, and the isolation tests below would then
		// report leaks that are really the test database's fault. Say which.
		t.Fatalf("open (ABHED_TEST_DSN must be a plain role, not a superuser): %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func newSession(t *testing.T, p *Postgres, id, tenant string) {
	t.Helper()
	err := p.CreateSession(context.Background(), SessionRecord{
		ID: id, Tenant: tenant, User: "tester", Workspace: "/w",
		Model: "test-model", Mode: "default", StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
}

func ev(sessionID string, seq int64, typ agent.EventType, trust agent.Trust, payload any) agent.Event {
	raw, _ := json.Marshal(payload)
	return agent.Event{
		ID:        fmt.Sprintf("%s-ev-%d", sessionID, seq),
		SessionID: sessionID,
		Seq:       seq,
		Type:      typ,
		Payload:   raw,
		Actor:     agent.ActorAgent,
		Trust:     trust,
		CreatedAt: time.Now().UTC(),
	}
}

func TestAppendAndReplay(t *testing.T) {
	p := openStore(t, "acme")
	id := fmt.Sprintf("s-replay-%d", time.Now().UnixNano())
	newSession(t, p, id, "acme")

	want := []agent.Event{
		ev(id, 1, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "fix the tests"}),
		ev(id, 2, agent.EvActionRequested, agent.Trusted, agent.ActionRequested{Tool: "bash"}),
		ev(id, 3, agent.EvObservation, agent.Untrusted, agent.Observation{Tool: "bash", Content: "exit 0"}),
	}
	for _, e := range want {
		if err := p.Append(e); err != nil {
			t.Fatalf("append seq %d: %v", e.Seq, err)
		}
	}

	got, err := p.Events(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("want %d events, got %d", len(want), len(got))
	}
	for i := range got {
		if got[i].Seq != want[i].Seq || got[i].Type != want[i].Type {
			t.Errorf("event %d mismatch: got %s/%d", i, got[i].Type, got[i].Seq)
		}
	}
	// Provenance must survive the round trip - injection defense depends on it.
	if got[2].Trust != agent.Untrusted {
		t.Fatalf("trust tag lost in persistence: %s", got[2].Trust)
	}
}

// Durability is the whole point: a new connection must see prior events.
func TestEventsSurviveReconnect(t *testing.T) {
	id := fmt.Sprintf("s-durable-%d", time.Now().UnixNano())

	p1 := openStore(t, "acme")
	newSession(t, p1, id, "acme")
	for i := int64(1); i <= 5; i++ {
		if err := p1.Append(ev(id, i, agent.EvAgentMessage, agent.Trusted, agent.Message{Text: "turn"})); err != nil {
			t.Fatal(err)
		}
	}
	p1.Close()

	p2 := openStore(t, "acme")
	got, err := p2.Events(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("events did not survive reconnect: got %d of 5", len(got))
	}
}

// An audit log you can edit is not an audit log.
func TestEventsAreImmutable(t *testing.T) {
	p := openStore(t, "acme")
	id := fmt.Sprintf("s-immutable-%d", time.Now().UnixNano())
	newSession(t, p, id, "acme")
	if err := p.Append(ev(id, 1, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "original"})); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	_, err := p.pool.Exec(ctx, `UPDATE events SET payload = '{"text":"tampered"}' WHERE session_id = $1`, id)
	if err == nil {
		t.Fatal("UPDATE on events must be rejected by the append-only trigger")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = p.pool.Exec(ctx, `DELETE FROM events WHERE session_id = $1`, id)
	if err == nil {
		t.Fatal("DELETE on events must be rejected")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("unexpected error: %v", err)
	}

	// And the original content is intact.
	got, _ := p.Events(id)
	if len(got) != 1 || !strings.Contains(string(got[0].Payload), "original") {
		t.Fatal("event content changed despite rejection")
	}
}

// Row-level security must isolate tenants even when the query does not filter.
func TestRowLevelSecurityIsolatesTenants(t *testing.T) {
	idA := fmt.Sprintf("s-acme-%d", time.Now().UnixNano())
	idB := fmt.Sprintf("s-globex-%d", time.Now().UnixNano())

	acme := openStore(t, "acme")
	newSession(t, acme, idA, "acme")
	if err := acme.Append(ev(idA, 1, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "acme secret"})); err != nil {
		t.Fatal(err)
	}

	globex := openStore(t, "globex")
	newSession(t, globex, idB, "globex")
	if err := globex.Append(ev(idB, 1, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "globex secret"})); err != nil {
		t.Fatal(err)
	}

	// Deliberately unfiltered query: RLS, not the WHERE clause, must isolate.
	rows, err := globex.pool.Query(context.Background(), `SELECT session_id, payload FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		var payload []byte
		rows.Scan(&sid, &payload)
		if strings.Contains(string(payload), "acme secret") {
			t.Fatalf("CROSS-TENANT LEAK: globex read acme's event %s", sid)
		}
	}

	// And the other direction.
	got, err := globex.Events(idA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("globex could replay acme's session: %d events", len(got))
	}

	// The owner still sees its own.
	own, err := acme.Events(idA)
	if err != nil || len(own) != 1 {
		t.Fatalf("owner lost access to its own events: %d, %v", len(own), err)
	}
}

func TestSinceSupportsResumption(t *testing.T) {
	p := openStore(t, "acme")
	id := fmt.Sprintf("s-since-%d", time.Now().UnixNano())
	newSession(t, p, id, "acme")
	for i := int64(1); i <= 10; i++ {
		p.Append(ev(id, i, agent.EvAgentMessage, agent.Trusted, agent.Message{Text: "x"}))
	}
	got, err := p.Since(id, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events after seq 7, got %d", len(got))
	}
	if got[0].Seq != 8 {
		t.Fatalf("resumption should start at 8, got %d", got[0].Seq)
	}
}

// Re-appending after a crash must be safe.
func TestDuplicateAppendIsIdempotent(t *testing.T) {
	p := openStore(t, "acme")
	id := fmt.Sprintf("s-dup-%d", time.Now().UnixNano())
	newSession(t, p, id, "acme")

	e := ev(id, 1, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "once"})
	if err := p.Append(e); err != nil {
		t.Fatal(err)
	}
	if err := p.Append(e); err != nil {
		t.Fatalf("duplicate append should succeed silently: %v", err)
	}
	got, _ := p.Events(id)
	if len(got) != 1 {
		t.Fatalf("duplicate created %d rows", len(got))
	}
}

func TestUnknownSessionGivesActionableError(t *testing.T) {
	p := openStore(t, "acme")
	err := p.Append(ev("s-does-not-exist", 1, agent.EvUserMessage, agent.Trusted, agent.Message{}))
	if err == nil {
		t.Fatal("expected an error for an unknown session")
	}
	if !strings.Contains(err.Error(), "CreateSession") {
		t.Fatalf("error should name the fix, got: %v", err)
	}
}

func TestSessionTotalsRecordedOnEnd(t *testing.T) {
	p := openStore(t, "acme")
	id := fmt.Sprintf("s-totals-%d", time.Now().UnixNano())
	newSession(t, p, id, "acme")

	p.Append(ev(id, 1, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "go"}))
	p.Append(ev(id, 2, agent.EvSessionEnded, agent.Trusted, agent.SessionEnded{
		Reason: agent.TermCompleted, Turns: 4,
		TokensIn: 5000, TokensOut: 300, TokensCached: 4000, Compactions: 1,
	}))

	rec, err := p.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.TerminalReason != string(agent.TermCompleted) {
		t.Fatalf("terminal reason not recorded: %q", rec.TerminalReason)
	}
	if rec.Turns != 4 || rec.TokensIn != 5000 || rec.TokensCached != 4000 || rec.Compactions != 1 {
		t.Fatalf("totals wrong: %+v", rec)
	}
	if rec.EndedAt == nil {
		t.Fatal("ended_at not set")
	}
}

func TestListSessionsScopedToTenant(t *testing.T) {
	acme := openStore(t, "acme")
	id := fmt.Sprintf("s-list-%d", time.Now().UnixNano())
	newSession(t, acme, id, "acme")

	globex := openStore(t, "globex")
	list, err := globex.ListSessions(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == id {
			t.Fatal("CROSS-TENANT LEAK: globex listed acme's session")
		}
	}
}

func TestSubscribeStreamsAppends(t *testing.T) {
	p := openStore(t, "acme")
	id := fmt.Sprintf("s-sub-%d", time.Now().UnixNano())
	newSession(t, p, id, "acme")

	ch := p.Subscribe(id)
	defer p.Unsubscribe(id, ch)

	go p.Append(ev(id, 1, agent.EvAgentMessage, agent.Trusted, agent.Message{Text: "hello"}))

	select {
	case got := <-ch:
		if got.Seq != 1 {
			t.Fatalf("wrong event: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("subscriber did not receive the append")
	}
}

func TestDSNRedaction(t *testing.T) {
	got := redactDSN("postgres://user:supersecret@host:5432/db")
	if strings.Contains(got, "supersecret") {
		t.Fatalf("password leaked: %s", got)
	}
	if !strings.Contains(got, "user") || !strings.Contains(got, "host:5432") {
		t.Fatalf("redaction removed too much: %s", got)
	}
}

// Scan is the seam an export stands on, so what it promises is pinned here:
// commit order within the window, nothing outside it, and nothing from
// another tenant even though the query names no tenant.
func TestScanStreamsWindowInOrder(t *testing.T) {
	p := openStore(t, "t-scan")
	other := openStore(t, "t-scan-other")
	id := testID(t, "sess-scan-")
	newSession(t, p, id, "t-scan")
	newSession(t, other, id+"-other", "t-scan-other")

	before := time.Now().UTC().Add(-time.Second)
	for seq := int64(1); seq <= 3; seq++ {
		if err := p.Append(ev(id, seq, agent.EvUserMessage, agent.Trusted,
			agent.Message{Text: fmt.Sprint("m", seq)})); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := other.Append(ev(id+"-other", 1, agent.EvUserMessage, agent.Trusted,
		agent.Message{Text: "theirs"})); err != nil {
		t.Fatalf("append other: %v", err)
	}

	var seen []int64
	err := p.Scan(context.Background(), before, time.Now().UTC().Add(time.Second),
		func(e agent.Event) error {
			if e.SessionID != id {
				t.Errorf("scan crossed a tenant: saw session %s", e.SessionID)
			}
			seen = append(seen, e.Seq)
			return nil
		})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(seen) != 3 || seen[0] != 1 || seen[2] != 3 {
		t.Errorf("scan returned seqs %v, want [1 2 3]", seen)
	}

	// A window that closes before this session was written holds none of its
	// events. Other sessions in the same tenant may sit in that window, so the
	// count is scoped to this one.
	n := 0
	if err := p.Scan(context.Background(), before.Add(-time.Hour), before,
		func(e agent.Event) error {
			if e.SessionID == id {
				n++
			}
			return nil
		}); err != nil || n != 0 {
		t.Errorf("window closing before the session was written: n=%d err=%v", n, err)
	}
}
