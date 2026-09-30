package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	id, err := copyBranch(ctx, pg, cfg, src, events, 0, "", false)
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

// The review's probe: a file resumed with -r on Postgres. Its payloads pass
// the vault's redactor before they are stored, since Postgres does not
// redact on append, and its events come in untrusted.
func TestBranchFromAFileIsRedactedOnPostgres(t *testing.T) {
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run the Postgres branch test")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ABHED_SECRETS_FILE", "")
	_ = os.MkdirAll(filepath.Join(home, ".abhed"), 0o700)
	const canary = "pg-branch-canary-5521"
	if err := os.WriteFile(filepath.Join(home, ".abhed", "secrets.json"), []byte(`{"CANARY":"`+canary+`"}`), 0o600); err != nil {
		t.Fatal(err)
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
	raw, _ := json.Marshal(agent.Message{Text: "the key is " + canary})
	file := []agent.Event{{ID: "f1", SessionID: "elsewhere", Seq: 1, Type: agent.EvUserMessage, Payload: raw,
		Actor: agent.ActorUser, Trust: agent.Trusted, CreatedAt: time.Now().UTC()}}
	id, err := copyBranch(ctx, pg, cfg, "elsewhere", file, 0, "", true)
	if err != nil {
		t.Fatal(err)
	}
	copied, _ := pg.Events(id)
	for _, ev := range copied {
		if strings.Contains(string(ev.Payload), canary) {
			t.Fatalf("the canary reached Postgres: %s", ev.Payload)
		}
		if ev.Type == agent.EvUserMessage && ev.Trust != agent.Untrusted {
			t.Fatalf("an event from a file came in %s", ev.Trust)
		}
	}
}
