package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/witness"
)

// A records folder put back from an older copy verifies on its own, since
// every line and head in it is genuine; the witness, which kept the later
// heads, catches it.
func TestWitnessCatchesARolledBackRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	heads := filepath.Join(t.TempDir(), "heads.jsonl")
	cfg := config.Config{Record: config.RecordConfig{Witness: config.WitnessConfig{File: heads}}, ManagedKeys: []string{"record.witness"}}
	ws := t.TempDir()
	run := func(id string, resume bool, text string) {
		rec, err := openRecord(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer rec.Close()
		r := agent.NewRecorder(rec, id, "")
		if resume {
			evs, _ := rec.Events(id)
			if ok, err := rec.ClaimResume(context.Background(), id); !ok || err != nil {
				t.Fatalf("claim: %v %v", ok, err)
			}
			r.Advance(evs[len(evs)-1].Seq)
		} else if err := rec.CreateSession(context.Background(), store.SessionRecord{ID: id, Workspace: ws, User: cliUser()}); err != nil {
			t.Fatal(err)
		}
		_, _ = r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: text})
		_, _ = r.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted, Turns: 1})
	}
	id := newConversationID()
	run(id, false, "first")
	records := filepath.Join(home, ".abhed", "records")
	backup := filepath.Join(t.TempDir(), "records")
	if out, err := exec.Command("cp", "-Rp", records, backup).CombinedOutput(); err != nil {
		t.Fatalf("copy: %v %s", err, out)
	}
	run(id, true, "second")
	if err := os.RemoveAll(records); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-Rp", backup, records).CombinedOutput(); err != nil {
		t.Fatalf("restore: %v %s", err, out)
	}
	var outs []string
	code, out, _ := runRecord(t, ws, "verify", "-witness", "")
	if code != 0 {
		t.Fatalf("the restored copy should verify on its own: %d %s", code, out)
	}
	outs = append(outs, out)
	if b, _ := os.ReadFile(heads); !strings.Contains(string(b), id) {
		t.Fatalf("no head of %s reached the witness: %s", id, b)
	}
	code, out, _ = runRecord(t, ws, "verify", "-witness", heads)
	if code == 0 || !strings.Contains(out, "FAILED  "+id) || !strings.Contains(out, "older copy was put back") {
		t.Fatalf("the witness did not catch the rollback: %d %s (%v)", code, out, outs)
	}
}

// In a Postgres deployment, record verify walks each session's chain and the
// heads the witness holds; a head the database no longer has fails.
func TestRecordVerifyPostgresChainAndWitness(t *testing.T) {
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run against Postgres")
	}
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	cfg := config.Config{}
	cfg.Storage.Driver, cfg.Storage.DSN, cfg.Storage.SingleRole = "postgres", dsn, true
	heads := filepath.Join(t.TempDir(), "heads.jsonl")
	w, err := witness.Open(witness.Config{File: heads, KeyFile: filepath.Join(t.TempDir(), "k")})
	if err != nil {
		t.Fatal(err)
	}
	sc := storeConfig(cfg)
	sc.Anchor = w.Postgres
	pg, err := store.Open(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	id := "sess-vpg-" + newConversationID()
	if err := pg.CreateSession(ctx, store.SessionRecord{ID: id, Tenant: "default", User: "u", Workspace: "/w", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	r := agent.NewRecorder(pg, id, "")
	_, _ = r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "hi"})
	_, _ = r.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted})
	var out, errb strings.Builder
	if code := verifyPostgres(cfg, []string{"-witness", heads, id}, &out, &errb); code != 0 || !strings.Contains(out.String(), "ok      "+id) {
		t.Fatalf("verify: %d %s %s", code, out.String(), errb.String())
	}
	// A witnessed head the database does not hold: as after a restore.
	w.Postgres("default", id, 9, "f00")
	out.Reset()
	if code := verifyPostgres(cfg, []string{"-witness", heads, id}, &out, &errb); code == 0 || !strings.Contains(out.String(), "FAILED  "+id) {
		t.Fatalf("a missing witnessed head passed: %s", out.String())
	}
}

// A session pruned or deleted on the record is reported as pruned against
// the witness, not as tampering; one removed with no tombstone still fails.
func TestWitnessAcceptsAPrunedSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	heads := filepath.Join(t.TempDir(), "heads.jsonl")
	cfg := config.Config{Record: config.RecordConfig{Witness: config.WitnessConfig{File: heads}}, ManagedKeys: []string{"record.witness"}}
	ws := t.TempDir()
	rec, err := openRecord(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range 3 {
		id := newConversationID()
		if err := rec.CreateSession(context.Background(), store.SessionRecord{ID: id, Workspace: ws, User: cliUser()}); err != nil {
			t.Fatal(err)
		}
		r := agent.NewRecorder(rec, id, "")
		_, _ = r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "hi"})
		_, _ = r.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted, Turns: 1})
		ids = append(ids, id)
		if err := rec.Release(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rec.Prune(ids[0], "user", "retention"); err != nil {
		t.Fatal(err)
	}
	if err := rec.DeleteSession(ids[1]); err != nil {
		t.Fatal(err)
	}
	third := rec.Path(ids[2])
	rec.Close()
	code, out, _ := runRecord(t, ws, "verify", "-witness", heads)
	if code != 0 || strings.Contains(out, "FAILED") || !strings.Contains(out, "pruned  "+ids[0]) || !strings.Contains(out, "pruned  "+ids[1]) {
		t.Fatalf("a pruned session failed against the witness: %d %s", code, out)
	}
	// A file taken away with no tombstone is still caught.
	if err := os.Remove(third); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runRecord(t, ws, "verify", "-witness", heads)
	if code == 0 || !strings.Contains(out, "FAILED  "+ids[2]) {
		t.Fatalf("a removed session passed: %d %s", code, out)
	}
}
