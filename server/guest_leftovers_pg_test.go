package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// waitingAdapter answers nothing until the run is cancelled.
type waitingAdapter struct{ stubAdapter }

func (waitingAdapter) Complete(ctx context.Context, _ model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

// Two servers share one Postgres; B restarts and sweeps console guests'
// leftovers. A's guest, running there with a fresh heartbeat, and A's guest's
// recent ended session survive; what B's earlier run held, an open session
// whose node stopped heartbeating, and an ended session older than a
// guest's hours go.
func TestGuestSweepSparesAnotherNodesLiveGuests(t *testing.T) {
	pa := openSharedPG(t)
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	cfg.Auth.Guest = true
	a := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: waitingAdapter{}, Store: pa, NodeID: "node-a",
		Registry: tools.NewRegistry(tools.Read{})})
	ended := make(chan string, 1)
	live, err := a.StartSession(context.Background(), StartSpec{Prompt: "wait", User: "guest:alive", Tenant: "acme",
		OnEnd: func(reason string, _ error) { ended <- reason }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.StopSession("acme", "guest:alive", live) })
	waitUntil(t, "A to hold the guest's session", func() bool { return pgHolder(t, live) == a.holder })

	tag := fmt.Sprint(time.Now().UnixNano())
	row := func(id, user, node, extra string) string {
		id = id + "-" + tag
		if err := pa.CreateSession(context.Background(), store.SessionRecord{ID: id, Tenant: "acme", User: user,
			StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		pgExec(t, "UPDATE sessions SET node_id = $2, node_seen_at = now() WHERE id = $1", id, node)
		pgExec(t, "UPDATE sessions SET "+extra[2:]+" WHERE id = $1", id)
		return id
	}
	recentA := row("recent-a", "guest:alive", "node-a#x", ", ended_at = now()")
	earlierB := row("earlier-b", "guest:gone-b", "node-b#before", ", ended_at = now()")
	staleOpen := row("stale-open", "guest:gone-c", "node-c#1", ", node_seen_at = now() - interval '10 minutes'")
	oldEnded := row("old-ended", "guest:gone-d", "node-d#1", ", ended_at = now(), started_at = now() - interval '9 hours'")
	notGuest := row("not-guest", "local:ana", "node-b#before", ", ended_at = now()")

	b := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}, Store: openSharedPG(t), NodeID: "node-b",
		Registry: tools.NewRegistry(tools.Read{})})
	if n := b.RemoveGuestLeftovers(context.Background(), 8*time.Hour, true); n < 3 {
		t.Fatalf("B removed %d leftovers, want at least the 3 dead guests' sessions", n)
	}
	gone := func(id string) bool {
		_, err := pa.GetSession(context.Background(), id)
		return err != nil
	}
	for _, id := range []string{earlierB, staleOpen, oldEnded} {
		if !gone(id) {
			t.Errorf("%s was kept", id)
		}
	}
	for _, id := range []string{live, recentA, notGuest} {
		if gone(id) {
			t.Errorf("%s was removed", id)
		}
	}
	select {
	case r := <-ended:
		t.Fatalf("A's live guest run ended (%s) after B's sweep", r)
	case <-time.After(200 * time.Millisecond):
	}
	a.mu.RLock()
	stillRunning := a.running[live] != nil
	a.mu.RUnlock()
	if !stillRunning {
		t.Fatal("A no longer runs its guest's session")
	}
}
