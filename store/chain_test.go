package store

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Each event links to the one before it in the database; a row the table's
// owner changes behind the triggers breaks the chain at that row.
func TestEventChainShowsAnEditedRow(t *testing.T) {
	p := openStore(t, "default")
	ctx := context.Background()
	id := testID(t, "sess-chain-")
	newSession(t, p, id, "default")
	for i := int64(1); i <= 4; i++ {
		if err := p.Append(ev(id, i, agent.EvAgentMessage, agent.Trusted, map[string]string{"text": "step"})); err != nil {
			t.Fatal(err)
		}
	}
	r, err := p.VerifyChain(ctx, id)
	if err != nil || !r.OK() || r.Rows != 4 || len(r.Head) != 64 {
		t.Fatalf("verify = %+v, %v; want 4 chained rows", r, err)
	}
	// As the owner, with the append-only trigger off: what only an owner can do.
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{
		`ALTER TABLE events DISABLE TRIGGER events_no_update`,
		`UPDATE events SET type = 'agent.edited' WHERE session_id = '` + id + `' AND seq = 3`,
		`ALTER TABLE events ENABLE TRIGGER events_no_update`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var bad int64
	if err := tx.QueryRow(ctx, `SELECT first_bad FROM abhed_chain_verify('events', 'session_id', $1, $2)`,
		id, eventChainColumns).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 3 {
		t.Fatalf("first bad position %d, want 3", bad)
	}
}

// The trigger sets the chain whatever the writer sends, and concurrent
// writers of one session still make one unbroken chain.
func TestEventChainHoldsUnderConcurrentWriters(t *testing.T) {
	p := openStore(t, "default")
	ctx := context.Background()
	id := testID(t, "sess-chainc-")
	newSession(t, p, id, "default")
	var wg sync.WaitGroup
	for i := int64(1); i <= 20; i++ {
		wg.Add(1)
		go func(i int64) {
			defer wg.Done()
			if err := p.Append(ev(id, i, agent.EvAgentMessage, agent.Trusted, map[string]int64{"n": i})); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if _, err := p.pool.Exec(ctx, `INSERT INTO events (id, session_id, tenant_id, seq, type, payload, actor, chain_pos, prev_hash, hash)
		VALUES ($1, $2, 'default', 21, 'x', '{}', 'agent', 1, 'forged', 'forged')`, id+"-forged", id); err != nil {
		t.Fatal(err)
	}
	r, err := p.VerifyChain(ctx, id)
	if err != nil || !r.OK() || r.Rows != 21 {
		t.Fatalf("verify = %+v, %v; want 21 chained rows", r, err)
	}
	if strings.Contains(r.Head, "forged") {
		t.Fatal("the writer set the chain")
	}
}

// A run's end hands the chain head to the anchor.
func TestRunEndAnchorsTheChainHead(t *testing.T) {
	type head struct {
		session string
		pos     int64
		hash    string
	}
	var got []head
	cfg := singleRoleConfig(testDSN(t))
	cfg.Anchor = func(_, session string, pos int64, hash string) { got = append(got, head{session, pos, hash}) }
	p, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	id := testID(t, "sess-anchor-")
	newSession(t, p, id, "default")
	_ = p.Append(ev(id, 1, agent.EvUserMessage, agent.Trusted, map[string]string{"text": "hi"}))
	_ = p.Append(ev(id, 2, agent.EvSessionEnded, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted}))
	r, _ := p.VerifyChain(context.Background(), id)
	if len(got) != 1 || got[0].session != id || got[0].pos != 2 || got[0].hash != r.Head {
		t.Fatalf("anchored %+v, want position 2 at %s", got, r.Head)
	}
}
