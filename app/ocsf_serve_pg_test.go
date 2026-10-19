package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
)

// Two servers on one Postgres record with the OCSF file on: one writes, and
// when it stops the other goes on from the cursors they share. Between the
// two files every event is written once, in order.
func TestServeOCSFOnPostgresHasOneWriter(t *testing.T) {
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run the Postgres OCSF test")
	}
	ctx := context.Background()
	tenant := "ocsf-" + newConversationID()
	open := func() *store.Postgres {
		sc := store.DefaultConfig(dsn)
		sc.SingleRole, sc.Tenant = true, tenant
		pg, err := store.Open(ctx, sc)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pg.Close)
		return pg
	}
	pgA, pgB := open(), open()
	dir := t.TempDir()
	cfgFor := func(name string) config.Config {
		cfg := config.Default()
		cfg.Storage.Tenant = tenant
		cfg.Record.OCSF = config.OCSFConfig{Enabled: true, Path: filepath.Join(dir, name+".jsonl")}
		return cfg
	}
	stopA, err := serveOCSF(cfgFor("a"), pgA)
	if err != nil {
		t.Fatal(err)
	}
	stopB, err := serveOCSF(cfgFor("b"), pgB)
	if err != nil {
		t.Fatal(err)
	}
	defer stopB()

	id := newConversationID()
	if err := recordSession(ctx, pgA, id, cfgFor("a")); err != nil {
		t.Fatal(err)
	}
	rec := agent.NewRecorder(pgA, id, "")
	say := func(n int) {
		for i := 0; i < n; i++ {
			if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "m"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	count := func(name string) int {
		data, _ := os.ReadFile(filepath.Join(dir, name+".jsonl"))
		return len(ocsfLines(t, string(data)))
	}
	wait := func(n int) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && count("a")+count("b") < n {
			time.Sleep(50 * time.Millisecond)
		}
	}
	say(3)
	wait(3)
	stopA()
	say(2)
	wait(5)
	a, b := count("a"), count("b")
	if a+b != 5 || a == 0 || b == 0 {
		t.Fatalf("a wrote %d and b %d; want 5 in all, each from one writer, both writing", a, b)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "b.jsonl"))
	got := ocsfLines(t, string(data))
	for i, l := range got {
		if l.Seq != int64(a+i+1) {
			t.Fatalf("b's event %d is seq %d, want %d", i, l.Seq, a+i+1)
		}
	}
}
