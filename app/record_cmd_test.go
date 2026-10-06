package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// recordHome gives a test its own home with a record holding sessions for
// workspace ws, and returns the store (closed) and the session ids.
func recordHome(t *testing.T, ws string, prompts ...string) []string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	rec, err := openRecord(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	var ids []string
	for _, p := range prompts {
		id := newConversationID()
		if err := rec.CreateSession(context.Background(), store.SessionRecord{ID: id, Workspace: ws, User: cliUser()}); err != nil {
			t.Fatal(err)
		}
		r := agent.NewRecorder(rec, id, "")
		_, _ = r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: p})
		_, _ = r.Record(agent.EvAgentMessage, agent.ActorAgent, agent.Trusted, agent.Message{Text: "done: " + p})
		_, _ = r.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted, Turns: 1})
		ids = append(ids, id)
	}
	return ids
}

func runRecord(t *testing.T, ws string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := recordCmd(ws, args, config.TrustRefused, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestRecordListShowVerify(t *testing.T) {
	ws := t.TempDir()
	ids := recordHome(t, ws, "fix the login bug", "write the docs")

	code, out, _ := runRecord(t, ws, "list")
	if code != 0 || !strings.Contains(out, ids[0]) || !strings.Contains(out, "write the docs") {
		t.Fatalf("list: %d %s", code, out)
	}
	if strings.Index(out, ids[1]) > strings.Index(out, ids[0]) {
		t.Fatalf("newest first: %s", out)
	}
	if code, out, _ := runRecord(t, t.TempDir(), "list"); code != 0 || strings.Contains(out, ids[0]) {
		t.Fatalf("another workspace's list: %s", out)
	}
	code, out, _ = runRecord(t, ws, "show", ids[0][:8])
	if code != 0 || !strings.Contains(out, "fix the login bug") || !strings.Contains(out, "session.ended") {
		t.Fatalf("show: %d %s", code, out)
	}
	code, out, _ = runRecord(t, ws, "verify")
	if code != 0 || strings.Count(out, "ok ") != 3 || !strings.Contains(out, "not proof against them") {
		t.Fatalf("verify: %d %s", code, out)
	}
	// The index is numbered by line; it used to say "head seq 0". Each session
	// has its create, title, active and end lines.
	if !strings.Contains(out, "ok      index: 8 lines, head line 8 ") || strings.Contains(out, "head seq 0") {
		t.Fatalf("verify names the index head by seq: %s", out)
	}
}

func TestRecordVerifyFailsOnATamperedLine(t *testing.T) {
	ws := t.TempDir()
	ids := recordHome(t, ws, "the original words")
	dir, _ := local.DefaultDir()
	p := filepath.Join(dir, "default", ids[0]+".jsonl")
	data, _ := os.ReadFile(p)
	_ = os.WriteFile(p, bytes.Replace(data, []byte("original"), []byte("replaced"), 1), 0o600)

	code, out, _ := runRecord(t, ws, "verify", ids[0])
	if code != 1 || !strings.Contains(out, "FAILED") || !strings.Contains(out, "at seq 1") {
		t.Fatalf("verify: %d %s", code, out)
	}
	if code, _, _ := runRecord(t, ws, "verify"); code != 1 {
		t.Fatal("verify of everything passed over a tampered session")
	}
	// The file named by its path is checked in the record, with its head.
	if code, out, _ := runRecord(t, ws, "verify", p); code != 1 || !strings.Contains(out, "FAILED") {
		t.Fatalf("verify by path: %d %s", code, out)
	}
	// It is not exported as sound: refused, or marked with -unverified.
	out = filepath.Join(t.TempDir(), "x.jsonl")
	if code, _, e := runRecord(t, ws, "export", ids[0], "-o", out); code == 0 || !strings.Contains(e, "-unverified") {
		t.Fatalf("export of a failing record: %d %s", code, e)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a refused export left a file")
	}
	if code, _, e := runRecord(t, ws, "export", ids[0], "-o", out, "-unverified"); code != 0 {
		t.Fatalf("export -unverified: %s", e)
	}
	if code, o, _ := runRecord(t, ws, "verify", out); code != 1 || !strings.Contains(o, "FAILED") {
		t.Fatalf("the marked export verified: %s", o)
	}
}

func TestRecordExportAndVerifyTheCopy(t *testing.T) {
	ws := t.TempDir()
	ids := recordHome(t, ws, "export me")
	code, _, errOut := runRecord(t, ws, "export", ids[0])
	if code != 0 {
		t.Fatalf("export: %s", errOut)
	}
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".abhed", "exports", ids[0]+".jsonl")
	if !strings.Contains(errOut, p) {
		t.Fatalf("export went to %s, want %s", errOut, p)
	}
	if entries, _ := os.ReadDir(ws); len(entries) != 0 {
		t.Fatal("the export was written into the workspace")
	}
	if code, out, _ := runRecord(t, ws, "verify", p); code != 0 || !strings.Contains(out, "ok") {
		t.Fatalf("verify the export: %d %s", code, out)
	}
	for _, f := range []string{"html", "txt"} {
		out := filepath.Join(t.TempDir(), "x."+f)
		if code, _, e := runRecord(t, ws, "export", ids[0], "-format", f, "-o", out); code != 0 {
			t.Fatalf("export %s: %s", f, e)
		}
		if data, _ := os.ReadFile(out); !bytes.Contains(data, []byte("export me")) {
			t.Fatalf("%s export: %s", f, data)
		}
	}
}

func TestRecordPruneNeedsConfirmAndLeavesATombstone(t *testing.T) {
	ws := t.TempDir()
	ids := recordHome(t, ws, "old work", "keep this")
	if code, _, errOut := runRecord(t, ws, "prune", ids[0]); code == 0 || !strings.Contains(errOut, "-yes") {
		t.Fatalf("prune without a terminal or -yes: %d %s", code, errOut)
	}
	code, out, _ := runRecord(t, ws, "prune", ids[0], "-yes")
	if code != 0 || !strings.Contains(out, "tombstone") {
		t.Fatalf("prune: %d %s", code, out)
	}
	if _, out, _ := runRecord(t, ws, "list"); strings.Contains(out, ids[0]) || !strings.Contains(out, ids[1]) {
		t.Fatalf("list after prune: %s", out)
	}
	if code, out, _ := runRecord(t, ws, "verify"); code != 0 {
		t.Fatalf("verify after prune: %s", out)
	}
	if code, out, _ := runRecord(t, ws, "prune", "-older-than", "30d", "-yes"); code != 0 || !strings.Contains(out, "nothing to prune") {
		t.Fatalf("nothing old enough: %d %s", code, out)
	}
	if code, _, _ := runRecord(t, ws, "prune"); code != 2 {
		t.Fatal("prune with nothing named")
	}
}

// A managed record.dir moves the record: sessions go there, the record
// command reads there, and the directory is state the agent cannot reach.
// The same key in the user's own file is set aside.
func TestManagedRecordDirTakesEffect(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "org-records")
	managedConfig(t, `{"record":{"dir":"`+dir+`"}}`)
	ws := t.TempDir()
	cfg, err := config.LoadWith(ws, config.LoadOptions{Quiet: true})
	if err != nil || cfg.Record.Dir != dir {
		t.Fatalf("record.dir = %q, %v", cfg.Record.Dir, err)
	}
	es, closeStore, err := openStore(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec := es.(*local.Store)
	if real, _ := filepath.EvalSymlinks(dir); rec.Dir() != real {
		t.Fatalf("the record opened at %s", rec.Dir())
	}
	_ = rec.CreateSession(context.Background(), store.SessionRecord{ID: "s-org", Workspace: ws})
	closeStore()
	if _, err := os.Stat(filepath.Join(dir, "default", "s-org.jsonl")); err != nil {
		t.Fatal("the session is not in the managed directory")
	}
	if code, out, _ := runRecord(t, ws, "list"); code != 0 || !strings.Contains(out, "s-org") {
		t.Fatalf("record list under the managed dir: %s", out)
	}
	registerState(cfg, ws)
	if !tools.IsState(filepath.Join(dir, "default", "s-org.jsonl"), ws) {
		t.Fatal("the managed record directory is not state")
	}

	managedConfig(t, "")
	home, _ := os.UserHomeDir()
	_ = os.MkdirAll(filepath.Join(home, ".abhed"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(`{"record":{"dir":"/tmp/elsewhere","retention_days":1}}`), 0o600)
	cfg, _ = config.LoadWith(ws, config.LoadOptions{Quiet: true})
	if cfg.Record.Dir != "" || cfg.Record.RetentionDays != 0 {
		t.Fatalf("the user's file moved or limited the record: %+v", cfg.Record)
	}
}

// A managed record.retention_days prunes older sessions when the record is
// opened, each leaving a tombstone.
func TestManagedRetentionPrunes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	managedConfig(t, `{"record":{"dir":"`+dir+`","retention_days":1}}`)
	cfg, err := config.LoadWith(t.TempDir(), config.LoadOptions{Quiet: true})
	if err != nil || !cfg.ManagedSets("record.retention_days") {
		t.Fatalf("retention not managed: %v", err)
	}
	// One session last used two days ago, one now.
	old, err := local.Open(local.Options{Dir: dir, Clock: func() time.Time { return time.Now().Add(-48 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	_ = old.CreateSession(context.Background(), store.SessionRecord{ID: "s-old", Workspace: dir})
	_ = old.Close()
	now, _ := local.Open(local.Options{Dir: dir})
	_ = now.CreateSession(context.Background(), store.SessionRecord{ID: "s-new", Workspace: dir})
	_ = now.Close()

	rec, err := openRecord(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	l, _ := rec.Index().List(local.Filter{All: true})
	if len(l) != 1 || l[0].ID != "s-new" {
		t.Fatalf("after retention: %+v", l)
	}
	if _, err := rec.Verify("s-old"); err == nil || !strings.Contains(err.Error(), "tombstone") {
		t.Fatalf("the pruned session left no tombstone: %v", err)
	}
}

// Verifying one session checks the index first, and a file in the records
// folder that the index does not know is not taken for an export.
func TestRecordVerifyOneChecksTheIndex(t *testing.T) {
	ws := t.TempDir()
	ids := recordHome(t, ws, "one", "two")
	dir, _ := local.DefaultDir()
	tenant := filepath.Join(dir, "default")
	data, _ := os.ReadFile(filepath.Join(tenant, ids[0]+".jsonl"))
	stray := filepath.Join(tenant, "s-unlisted.jsonl")
	_ = os.WriteFile(stray, data, 0o600)
	if code, out, _ := runRecord(t, ws, "verify", stray); code != 1 || !strings.Contains(out, "index has no such session") {
		t.Fatalf("an unlisted record file: %d %s", code, out)
	}
	idx := filepath.Join(tenant, "index.jsonl")
	data, _ = os.ReadFile(idx)
	_ = os.WriteFile(idx, bytes.Replace(data, []byte(`"op":"title"`), []byte(`"op":"TITLE"`), 1), 0o600)
	if code, out, _ := runRecord(t, ws, "verify", ids[1]); code != 1 || !strings.Contains(out, "FAILED  index") {
		t.Fatalf("one session over a damaged index: %d %s", code, out)
	}
}

// The prune and push confirmations go ahead only on their Yes number: Enter,
// letters and other numbers ask again, and input that ends refuses.
func TestConfirmNumbered(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"2\n", true},
		{"1\n", false},
		{"\n\ny\nyes\n3\n0\n2\n", true},
		{"y\n", false},
		{"Y\nyes\n", false},
		{"", false},
		{"\n1\n", false},
	}
	for _, c := range cases {
		var out bytes.Buffer
		if got := confirmNumbered(strings.NewReader(c.in), &out, "Prune it?", "Yes, prune"); got != c.want {
			t.Errorf("%q: got %v, want %v\n%s", c.in, got, c.want, out.String())
		}
		if !strings.Contains(out.String(), "1. No (keep)\n  2. Yes, prune\nanswer 1-2: ") {
			t.Errorf("%q: not numbered with No first:\n%s", c.in, out.String())
		}
	}
}
