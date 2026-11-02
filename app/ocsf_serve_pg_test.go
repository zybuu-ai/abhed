package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/ocsf"
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

// A saved place in commit order that cannot be read does not stop the feed:
// it goes on from the end, saved, and the next poll reads on from there.
func TestCommitFeedRecoversAnUnreadablePlace(t *testing.T) {
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run the Postgres OCSF test")
	}
	ctx := context.Background()
	sc := store.DefaultConfig(dsn)
	sc.SingleRole, sc.Tenant = true, "ocsf-bad-"+newConversationID()
	pg, err := store.Open(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	id := newConversationID()
	if err := pg.CreateSession(ctx, store.SessionRecord{ID: id, User: "u", Workspace: "/w", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	rec := agent.NewRecorder(pg, id, "")
	if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "before"}); err != nil {
		t.Fatal(err)
	}
	w := pg.OCSFWriter("feed-" + newConversationID())
	defer w.Close()
	if err := w.SavePos("1/1/1/s"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"garbage", "99999999999999999999/1/1/s"} {
		if _, err := pg.Pool().Exec(ctx, `UPDATE ocsf_cursors SET commit_pos = $1 WHERE commit_pos <> '' AND session_id = ''
			AND tenant_id = $2`, bad, sc.Tenant); err != nil {
			t.Fatal(err)
		}
		feed := commitFeed(pg, w)
		if lines, done, err := feed(); err != nil || len(lines) != 0 || done != nil {
			t.Fatalf("over %q: %d lines, %v", bad, len(lines), err)
		}
		end, err := pg.CommitEnd(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := w.LoadPos(); got != end.String() {
			t.Fatalf("over %q the place is %q, want the end %q", bad, got, end.String())
		}
		// From there the feed reads what is written next.
		if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "after"}); err != nil {
			t.Fatal(err)
		}
		// A row is read once no older transaction is still open; another test's may be.
		var lines []ocsf.Line
		var save func() error
		for deadline := time.Now().Add(20 * time.Second); len(lines) == 0 && err == nil && time.Now().Before(deadline); {
			if lines, save, err = feed(); len(lines) == 0 {
				time.Sleep(50 * time.Millisecond)
			}
		}
		if err != nil || len(lines) != 1 || save == nil {
			t.Fatalf("after recovering from %q: %d lines, %v", bad, len(lines), err)
		}
		if err := save(); err != nil {
			t.Fatal(err)
		}
	}
}
