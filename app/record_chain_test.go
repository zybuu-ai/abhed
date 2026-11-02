package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/witness"
)

// A Postgres session exported with record export verifies on its own, with
// no database configured, and every tamper with the file fails, a cut one
// against the witness's heads as well as its trailer.
func TestRecordExportPostgresChainVerifiesOffline(t *testing.T) {
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
	id := "sess-xpg-" + newConversationID()
	if err := pg.CreateSession(ctx, store.SessionRecord{ID: id, Tenant: "default", User: "u", Workspace: "/w", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	r := agent.NewRecorder(pg, id, "")
	for _, text := range []string{"one", "two", "three"} {
		_, _ = r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: text})
	}
	_, _ = r.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted})

	file := filepath.Join(t.TempDir(), "s.chain.jsonl")
	var out, errb strings.Builder
	if code := exportPostgres(cfg, t.TempDir(), []string{id, "-o", file}, &out, &errb); code != 0 {
		t.Fatalf("export: %d %s", code, errb.String())
	}
	// Checked with no database: by the local record's command, and by the
	// Postgres one pointed at a server that is not there.
	ws := t.TempDir()
	nowhere := config.Config{}
	nowhere.Storage.Driver, nowhere.Storage.DSN = "postgres", "postgres://nobody@127.0.0.1:1/none?connect_timeout=1"
	verify := func(path string, extra ...string) (int, string) {
		var o, e strings.Builder
		args := append(append([]string{"verify"}, extra...), path)
		code := recordCmd(ws, args, config.TrustRefused, strings.NewReader(""), &o, &e)
		var po, pe strings.Builder
		if pcode := verifyPostgres(nowhere, args[1:], &po, &pe); pcode != code {
			t.Fatalf("the two commands disagree on %s: %d %s / %d %s %s", path, code, o.String(), pcode, po.String(), pe.String())
		}
		return code, o.String() + e.String()
	}
	if code, s := verify(file, "-witness", heads, "-witness-key", keyFile); code != 0 || !strings.Contains(s, "ok      ") || !strings.Contains(s, "1 witnessed head(s)") {
		t.Fatalf("an untouched export: %d %s", code, s)
	}
	head, err := pg.EventChain().Head(ctx, id)
	if err != nil || head.Pos != 4 {
		t.Fatalf("head = %+v, %v", head, err)
	}
	data, _ := os.ReadFile(file)
	lines := strings.SplitAfter(strings.TrimSuffix(string(data), "\n"), "\n") // header, 4 links, trailer
	write := func(name string, ls []string) string {
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte(strings.Join(ls, "")), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cut := append(append([]string{}, lines[:3]...), lines[5])
	cases := map[string]struct {
		path  string
		extra []string
	}{
		"edit":                   {write("edit", append(append(append([]string{}, lines[:2]...), strings.Replace(lines[2], "user.message", "user.messagf", 1)), lines[3:]...)), nil},
		"delete":                 {write("delete", append(append([]string{}, lines[:2]...), lines[3:]...)), nil},
		"reorder":                {write("reorder", append(append(append([]string{}, lines[:2]...), lines[3], lines[2]), lines[4:]...)), nil},
		"truncate":               {write("truncate", lines[:4]), nil},
		"truncate, trailer kept": {write("cut", cut), nil},
		"truncate, witnessed":    {write("cutw", lines[:3]), []string{"-witness", heads, "-witness-key", keyFile}},
		"truncate, head given":   {write("cuth", lines[:3]), []string{"-head", strconv.FormatInt(head.Pos, 10) + ":" + head.Hash}},
	}
	for name, c := range cases {
		if code, s := verify(c.path, c.extra...); code == 0 || !strings.Contains(s, "FAILED") {
			t.Errorf("%s passed: %s", name, s)
		}
	}
}
