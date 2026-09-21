package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func routingStore(t *testing.T) (*Postgres, context.Context) {
	t.Helper()
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run routing tests")
	}
	ctx := context.Background()
	pg, err := Open(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg, ctx
}

func seed(t *testing.T, pg *Postgres, ctx context.Context, id string) {
	t.Helper()
	if err := pg.CreateSession(ctx, SessionRecord{
		ID: id, Tenant: "default", User: "u", Workspace: "/w",
		Model: "m", Mode: "default", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// A claim has to survive a read from another process, or routing sends the
// request back to a node that does not hold the session.
func TestClaimIsVisibleToAnotherReader(t *testing.T) {
	pg, ctx := routingStore(t)
	id := "s-route-" + time.Now().Format("150405.000000")
	seed(t, pg, ctx, id)

	if err := pg.ClaimNode(ctx, id, "node-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, err := pg.NodeFor(ctx, id, time.Minute)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "node-a" {
		t.Fatalf("NodeFor = %q, want node-a", got)
	}
}

// A node that dies must not strand its sessions: past the staleness window
// the claim reads as absent so any node may serve it.
func TestStaleClaimReadsAsUnheld(t *testing.T) {
	pg, ctx := routingStore(t)
	id := "s-stale-" + time.Now().Format("150405.000000")
	seed(t, pg, ctx, id)

	if err := pg.ClaimNode(ctx, id, "node-dead"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// A window of zero makes any claim stale immediately.
	got, err := pg.NodeFor(ctx, id, 0)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "" {
		t.Fatalf("NodeFor = %q, want empty for a stale claim", got)
	}
}

// Release is scoped to the claiming node. A slow release from node A must
// not clear a claim node B has since taken, or B's requests get misrouted.
func TestReleaseDoesNotClearAnotherNodesClaim(t *testing.T) {
	pg, ctx := routingStore(t)
	id := "s-rel-" + time.Now().Format("150405.000000")
	seed(t, pg, ctx, id)

	if err := pg.ClaimNode(ctx, id, "node-b"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Node A, long gone, releases late.
	if err := pg.ReleaseNode(ctx, id, "node-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	got, _ := pg.NodeFor(ctx, id, time.Minute)
	if got != "node-b" {
		t.Fatalf("NodeFor = %q after a foreign release, want node-b", got)
	}
}

// An unclaimed session reports no node, which is what tells the server to
// serve it here rather than redirect.
func TestUnclaimedSessionHasNoNode(t *testing.T) {
	pg, ctx := routingStore(t)
	id := "s-free-" + time.Now().Format("150405.000000")
	seed(t, pg, ctx, id)

	got, err := pg.NodeFor(ctx, id, time.Minute)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "" {
		t.Fatalf("NodeFor = %q, want empty", got)
	}
}
