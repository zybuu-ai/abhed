package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func TestPostgresOfFindsTheStoreBehindAWrapper(t *testing.T) {
	p := &Postgres{}
	if got, ok := PostgresOf(p); !ok || got != p {
		t.Fatal("a Postgres store itself")
	}
	if got, ok := PostgresOf(p.HeldBy("node", nil)); !ok || got != p {
		t.Fatal("a held store")
	}
	if got, ok := PostgresOf(named{p}); !ok || got != p {
		t.Fatal("a wrapper that names its store")
	}
	if _, ok := PostgresOf(struct{}{}); ok {
		t.Fatal("something else")
	}
	var none *Postgres
	if _, ok := PostgresOf(none); ok {
		t.Fatal("a nil store")
	}
}

type named struct{ p *Postgres }

func (n named) PostgresStore() *Postgres { return n.p }

func commitIDs(t *testing.T, p *Postgres, pos CommitPos) ([]string, CommitPos) {
	t.Helper()
	var ids []string
	err := p.ScanCommitted(context.Background(), pos, 100, func(ev agent.Event, at CommitPos) error {
		ids = append(ids, ev.ID)
		pos = at
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids, pos
}

// A row written in a transaction that began first but commits last is read
// in its place in commit order, and nothing after it is read while it is
// open, so a reader that has moved on never passes over it.
func TestScanCommittedNeverPassesAnOpenTransaction(t *testing.T) {
	tenant := fmt.Sprintf("t-commit-%d", time.Now().UnixNano())
	p := openStore(t, tenant)
	ctx := context.Background()
	if err := p.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	newSession(t, p, tenant+"-a", tenant)
	newSession(t, p, tenant+"-b", tenant)
	if err := p.Append(ev(tenant+"-b", 1, agent.EvSessionStarted, agent.Trusted, map[string]any{})); err != nil {
		t.Fatal(err)
	}
	ids, pos := commitIDs(t, p, CommitPos{})
	if len(ids) != 1 || ids[0] != tenant+"-b-ev-1" {
		t.Fatalf("first read %v", ids)
	}
	back, err := ParseCommitPos(pos.String())
	if err != nil || back.String() != pos.String() {
		t.Fatalf("position round trip %+v %+v %v", pos, back, err)
	}

	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events (id, session_id, tenant_id, seq, type, payload, actor, trust, created_at)
		VALUES ($1, $2, $3, 1, 'session.started', '{}', 'system', 'trusted', $4)`,
		tenant+"-a-ev-1", tenant+"-a", tenant, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := p.Append(ev(tenant+"-b", 2, agent.EvUserMessage, agent.Trusted, map[string]any{})); err != nil {
		t.Fatal(err)
	}
	if ids, _ := commitIDs(t, p, pos); len(ids) != 0 {
		t.Fatalf("read past an open transaction: %v", ids)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	ids, end := commitIDs(t, p, pos)
	if len(ids) != 2 || ids[0] != tenant+"-a-ev-1" || ids[1] != tenant+"-b-ev-2" {
		t.Fatalf("after the commit %v", ids)
	}
	if ids, _ := commitIDs(t, p, end); len(ids) != 0 {
		t.Fatalf("read again %v", ids)
	}
	other := openStore(t, tenant+"-other")
	if ids, _ := commitIDs(t, other, CommitPos{}); len(ids) != 0 {
		t.Fatalf("another tenant read %v", ids)
	}
}
