package app

import (
	"context"
	"os"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
)

// A branch on Postgres: every copied event gets its own id, since event
// ids are the events table's primary key, and the copy is whole.
func TestBranchCopyOnPostgres(t *testing.T) {
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run the Postgres branch test")
	}
	ctx := context.Background()
	sc := store.DefaultConfig(dsn)
	sc.SingleRole, sc.Tenant = true, "branch-test"
	pg, err := store.Open(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	cfg := config.Default()
	cfg.Storage.Tenant = "branch-test"
	src := newConversationID()
	if err := recordSession(ctx, pg, src, cfg); err != nil {
		t.Fatal(err)
	}
	rec := agent.NewRecorder(pg, src, "")
	for _, text := range []string{"one", "two"} {
		if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	events, _ := pg.Events(src)
	id, err := copyBranch(ctx, pg, cfg, src, events, 0, "")
	if err != nil {
		t.Fatalf("branch on Postgres: %v", err)
	}
	copied, _ := pg.Events(id)
	if len(copied) != 3 || copied[0].Type != agent.EvSessionBranched {
		t.Fatalf("branch holds %d events", len(copied))
	}
	for i, ev := range copied[1:] {
		if ev.ID == events[i].ID {
			t.Fatalf("event %d kept its source id", i)
		}
	}
}
