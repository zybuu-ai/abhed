package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	keyFile := filepath.Join(t.TempDir(), "k")
	w, err := witness.Open(witness.Config{File: heads, KeyFile: keyFile})
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
	if code := verifyPostgres(cfg, []string{"-witness", heads, "-witness-key", keyFile, id}, &out, &errb); code != 0 || !strings.Contains(out.String(), "ok      "+id) {
		t.Fatalf("verify: %d %s %s", code, out.String(), errb.String())
	}
	// Without the installation's key the heads are not checked at all.
	if code := verifyPostgres(cfg, []string{"-witness", heads, id}, &out, &errb); code == 0 || !strings.Contains(errb.String(), "-witness-key") {
		t.Fatalf("heads checked with no key: %d %s", code, errb.String())
	}
	// A head signed by another key, though it matches the database, fails.
	forged := filepath.Join(t.TempDir(), "forged.jsonl")
	fw, err := witness.Open(witness.Config{File: forged, KeyFile: filepath.Join(t.TempDir(), "other")})
	if err != nil {
		t.Fatal(err)
	}
	h, ok, err := pg.ChainHashAt(ctx, id, 2)
	if err != nil || !ok {
		t.Fatalf("head: %v %v", ok, err)
	}
	fw.Postgres("default", id, 2, h)
	out.Reset()
	if code := verifyPostgres(cfg, []string{"-witness", forged, "-witness-key", keyFile, id}, &out, &errb); code == 0 ||
		!strings.Contains(out.String(), "not signed by this installation") {
		t.Fatalf("a head under another key passed: %s", out.String())
	}
	// A witnessed head the database does not hold: as after a restore.
	w.Postgres("default", id, 9, "f00")
	out.Reset()
	if code := verifyPostgres(cfg, []string{"-witness", heads, "-witness-key", keyFile, id}, &out, &errb); code == 0 || !strings.Contains(out.String(), "FAILED  "+id) {
		t.Fatalf("a missing witnessed head passed: %s", out.String())
	}
}

// Failures to reach the witness are logged as they happen, the first at
// once and then at most one per interval, saying how many were not logged.
func TestWitnessReporterLogsAtOnceThenThrottles(t *testing.T) {
	var buf bytes.Buffer
	rep := newWitnessReporter(slog.New(slog.NewTextHandler(&buf, nil)), 50*time.Millisecond)
	report := rep.report
	report(errors.New("first"))
	if !strings.Contains(buf.String(), "first") {
		t.Fatalf("the first failure was not logged at once: %q", buf.String())
	}
	report(errors.New("second"))
	report(errors.New("third"))
	if strings.Contains(buf.String(), "second") || strings.Contains(buf.String(), "third") {
		t.Fatalf("failures within the interval were logged: %q", buf.String())
	}
	time.Sleep(60 * time.Millisecond)
	report(errors.New("fourth"))
	if !strings.Contains(buf.String(), "fourth") || !strings.Contains(buf.String(), "failures_not_logged=2") {
		t.Fatalf("after the interval: %q", buf.String())
	}
}

// Failures the reporter held back are counted in the log when the witness
// closes, not lost with the process.
func TestWitnessCloseLogsHeldBackFailures(t *testing.T) {
	var buf bytes.Buffer
	rep := newWitnessReporter(slog.New(slog.NewTextHandler(&buf, nil)), time.Hour)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	w, err := witness.Open(witness.Config{URL: dead.URL, KeyFile: filepath.Join(t.TempDir(), "witness.key"), OnError: rep.report})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		w.Local("default", "s1", int64(i+1), 0, "h")
	}
	closeWitness(&openedWitness{Witness: w, rep: rep})
	if !strings.Contains(buf.String(), "failures_not_logged=2") {
		t.Fatalf("held-back failures were lost at close: %q", buf.String())
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
	for range 4 {
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
	// Nor does a prune afterwards launder it, or a record edited before its prune.
	if code, out, errOut := runRecord(t, ws, "prune", "-yes", ids[2]); code != 0 {
		t.Fatalf("prune of the removed session: %d %s %s", code, out, errOut)
	}
	b, err := os.ReadFile(rec.Path(ids[3]))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec.Path(ids[3]), []byte(strings.Replace(string(b), `"hi"`, `"ho"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runRecord(t, ws, "prune", "-yes", ids[3]); code != 0 {
		t.Fatalf("prune of the edited session: %d %s %s", code, out, errOut)
	}
	code, out, _ = runRecord(t, ws, "verify", "-witness", heads)
	if code == 0 || !strings.Contains(out, "FAILED  "+ids[2]+": the witness holds line 2, and the session's file was gone before it was pruned") ||
		!strings.Contains(out, "FAILED  "+ids[3]+": the witness holds line 2, and the record did not verify when it was pruned") ||
		!strings.Contains(out, "pruned  "+ids[0]) {
		t.Fatalf("a prune after the damage passed: %d %s", code, out)
	}
}

// With no session named, verify says when it walked only the most recent
// sessions rather than all of them.
func TestRecordVerifyPostgresSaysWhenCapped(t *testing.T) {
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run against Postgres")
	}
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	cfg := config.Config{}
	cfg.Storage.Driver, cfg.Storage.DSN, cfg.Storage.SingleRole = "postgres", dsn, true
	pg, err := store.Open(ctx, storeConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	for range 2 {
		if err := pg.CreateSession(ctx, store.SessionRecord{ID: "sess-cap-" + newConversationID(), Tenant: "default", User: "u", Workspace: "/w", Model: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	old := pgVerifyLimit
	pgVerifyLimit = 1
	t.Cleanup(func() { pgVerifyLimit = old })
	var out, errb strings.Builder
	verifyPostgres(cfg, nil, &out, &errb)
	if !strings.Contains(out.String(), "only the 1 most recent sessions were checked") {
		t.Fatalf("a capped verify did not say so: %s %s", out.String(), errb.String())
	}
}

// With no session named, verify walks deleted and subagent sessions too, and
// says which they are.
func TestRecordVerifyPostgresWalksDeletedAndSubagents(t *testing.T) {
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run against Postgres")
	}
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	cfg := config.Config{}
	cfg.Storage.Driver, cfg.Storage.DSN, cfg.Storage.SingleRole = "postgres", dsn, true
	pg, err := store.Open(ctx, storeConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	parent, child, gone := "sess-walk-"+newConversationID(), "sess-walk-"+newConversationID(), "sess-walk-"+newConversationID()
	for _, id := range []string{parent, gone} {
		if err := pg.CreateSession(ctx, store.SessionRecord{ID: id, Tenant: "default", User: "u", Workspace: "/w", Model: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pg.CreateSubagentSession(ctx, child, parent, "a helper"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{parent, child, gone} {
		r := agent.NewRecorder(pg, id, "")
		_, _ = r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "hi"})
	}
	if err := pg.DeleteSession(gone); err != nil {
		t.Fatal(err)
	}
	var out, errb strings.Builder
	if code := verifyPostgres(cfg, nil, &out, &errb); code != 0 {
		t.Fatalf("verify: %d %s %s", code, out.String(), errb.String())
	}
	lines := map[string]string{}
	for _, l := range strings.Split(out.String(), "\n") {
		if id, _, ok := strings.Cut(strings.TrimPrefix(l, "ok      "), ":"); ok && strings.HasPrefix(l, "ok      ") {
			lines[id] = l
		}
	}
	for id, suffix := range map[string]string{parent: "", child: " (a subagent's)", gone: " (deleted, erased by key)"} {
		l, ok := lines[id]
		if !ok || !strings.HasSuffix(l, suffix) || suffix == "" && strings.HasSuffix(l, ")") {
			t.Errorf("verify did not report %s as %q:\n%s", id, suffix, out.String())
		}
	}
}

// A deleted Postgres session keeps its chained rows, erased by key, so the
// witness's heads for it still verify.
func TestRecordVerifyPostgresWitnessAfterDelete(t *testing.T) {
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run against Postgres")
	}
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	cfg := config.Config{}
	cfg.Storage.Driver, cfg.Storage.DSN, cfg.Storage.SingleRole = "postgres", dsn, true
	heads := filepath.Join(t.TempDir(), "heads.jsonl")
	keyFile := filepath.Join(t.TempDir(), "k")
	w, err := witness.Open(witness.Config{File: heads, KeyFile: keyFile})
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
	id := "sess-del-" + newConversationID()
	if err := pg.CreateSession(ctx, store.SessionRecord{ID: id, Tenant: "default", User: "u", Workspace: "/w", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	r := agent.NewRecorder(pg, id, "")
	_, _ = r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "hi"})
	_, _ = r.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted})
	if err := pg.DeleteSession(id); err != nil {
		t.Fatal(err)
	}
	var out, errb strings.Builder
	if code := verifyPostgres(cfg, []string{"-witness", heads, "-witness-key", keyFile, id}, &out, &errb); code != 0 || strings.Contains(out.String(), "FAILED") {
		t.Fatalf("a deleted session failed against the witness: %d %s %s", code, out.String(), errb.String())
	}
	// With no session named, deleted and subagent sessions are walked too, and said to be.
	child := "sess-sub-" + newConversationID()
	if err := pg.CreateSubagentSession(ctx, child, id, "helper"); err != nil {
		t.Fatal(err)
	}
	cr := agent.NewRecorder(pg, child, "")
	_, _ = cr.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "hi"})
	out.Reset()
	verifyPostgres(cfg, nil, &out, &errb)
	if !strings.Contains(out.String(), "ok      "+id+": 2 chained event(s)") || !strings.Contains(out.String(), "(deleted, erased by key)") ||
		!strings.Contains(out.String(), "ok      "+child+": 1 chained event(s)") || !strings.Contains(out.String(), "(a subagent's)") {
		t.Fatalf("verify left out the deleted or the subagent session: %s %s", out.String(), errb.String())
	}
}
