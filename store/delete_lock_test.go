package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// An append racing a delete either lands before the tree is locked, where a
// check under the locks sees it, or waits for the delete and is refused: it
// never lands in a deleted session.
func TestDeleteUnderTheTreeLocksRefusesAWaitingAppend(t *testing.T) {
	tenant := fmt.Sprintf("t-dellock-%d", time.Now().UnixNano())
	p := openStore(t, tenant)
	ctx := context.Background()
	if err := p.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id := tenant + "-s"
	newSession(t, p, id, tenant)
	if err := p.Append(ev(id, 1, agent.EvSessionStarted, agent.Trusted, map[string]any{})); err != nil {
		t.Fatal(err)
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ids, err := p.LockSessionTree(ctx, tx, id)
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("lock: %v %v", ids, err)
	}
	appended := make(chan error, 1)
	go func() {
		appended <- p.Append(ev(id, 2, agent.EvUserMessage, agent.Trusted, map[string]any{"text": "late"}))
	}()
	select {
	case err := <-appended:
		t.Fatalf("an append went through a locked tree: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// The check a caller makes under the locks sees only what was there.
	var last int64
	if err := tx.QueryRow(ctx, `SELECT max(seq) FROM events WHERE session_id = $1`, id).Scan(&last); err != nil || last != 1 {
		t.Fatalf("under the lock: last seq %d %v", last, err)
	}
	if err := p.DeleteSessionTx(ctx, tx, ids); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	p.Forget(ids)
	if err := <-appended; !errors.Is(err, ErrSessionDeleted) {
		t.Fatalf("the waiting append: %v; want ErrSessionDeleted", err)
	}
	var n int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows in the deleted session: %d %v", n, err)
	}
}
