package server

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
)

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
