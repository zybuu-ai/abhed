package store

import (
	"context"
	"errors"
	"testing"

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
		if err := p.CreateSubSession(ctx, child, parent, "subtask"); err != nil {
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
