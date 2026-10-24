package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A subagent's row names its parent: the list leaves it out in the query,
// before the limit, so a fan-out cannot push the owner's own sessions out;
// it is still found by id; and deleting the parent marks it too, keeping its
// events for the audit as the parent's are kept.
func TestSubagentRowsFollowTheirParent(t *testing.T) {
	p := openStore(t, "t-sub")
	ctx := context.Background()
	parent := testID(t, "sess-parent-")
	newSession(t, p, parent, "t-sub")
	var children []string
	for i := 0; i < 3; i++ {
		child := testID(t, "sess-child-")
		if err := p.CreateSubagentSession(ctx, child, parent, "subtask"); err != nil {
			t.Fatal(err)
		}
		if err := p.Append(ev(child, 1, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "work"})); err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}

	recs, err := p.ListSessions(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].ID != parent || recs[0].ParentID != "" {
		t.Fatalf("the newest listed session is %+v, want the parent", recs)
	}
	got, err := p.GetSession(ctx, children[0])
	if err != nil || got.ParentID != parent {
		t.Fatalf("child by id: %+v, %v", got, err)
	}

	if err := p.DeleteSession(parent); err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if _, err := p.GetSession(ctx, child); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a subagent outlived its deleted parent: %v", err)
		}
		if evs, _ := p.Events(child); len(evs) != 0 {
			t.Fatalf("a deleted parent's subagent record is still served: %d events", len(evs))
		}
		var kept int
		if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1`, child).Scan(&kept); err != nil || kept != 1 {
			t.Fatalf("the subagent's events were not kept for the audit: %d, %v", kept, err)
		}
	}
}

// A subagent's session is not made under a deleted parent, and one made
// while the parent's delete takes its locks is found and deleted with it.
func TestNoSubagentSessionUnderADeletedParent(t *testing.T) {
	p := openStore(t, "default")
	ctx := context.Background()
	parent := testID(t, "sess-par-")
	newSession(t, p, parent, "default")
	if err := p.DeleteSession(parent); err != nil {
		t.Fatal(err)
	}
	if err := p.CreateSubagentSession(ctx, testID(t, "sess-kid-"), parent, "late"); !errors.Is(err, ErrSessionDeleted) {
		t.Fatalf("a subagent under a deleted parent: %v", err)
	}

	// The delete holds the tree's locks: a creation waits, then is refused.
	parent2 := testID(t, "sess-par2-")
	newSession(t, p, parent2, "default")
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ids, err := p.LockSessionTree(ctx, tx, parent2)
	if err != nil {
		t.Fatal(err)
	}
	made := make(chan error, 1)
	go func() { made <- p.CreateSubagentSession(ctx, testID(t, "sess-kid2-"), parent2, "racing") }()
	time.Sleep(300 * time.Millisecond)
	if err := p.DeleteSessionTx(ctx, tx, ids); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-made; !errors.Is(err, ErrSessionDeleted) {
		t.Fatalf("a subagent made during the delete: %v", err)
	}

	// A creation that holds the parent's lock first commits, and the delete,
	// which read the tree before it, finds the new session on reading again.
	parent3 := testID(t, "sess-par3-")
	newSession(t, p, parent3, "default")
	kid := testID(t, "sess-kid3-")
	ctx3, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ctx3.Rollback(ctx) }()
	if _, err := ctx3.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('events/' || $1, 0))`, parent3); err != nil {
		t.Fatal(err)
	}
	if _, err := ctx3.Exec(ctx, `INSERT INTO sessions (id, tenant_id, user_id, workspace, model, prompt_hash, harness_version, mode, parent_id, started_at)
		VALUES ($1, 'default', 'u', '/w', 'subagent', '', '', 'auto', $2, now())`, kid, parent3); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- p.DeleteSession(parent3) }()
	time.Sleep(300 * time.Millisecond)
	if err := ctx3.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	var gone bool
	if err := p.pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM sessions WHERE id = $1`, kid).Scan(&gone); err != nil || !gone {
		t.Fatalf("the subagent made as the delete began was left: %v %v", gone, err)
	}
}
