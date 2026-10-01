package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
)

// serve with the memory driver keeps sessions in memory, as documented: none
// reach the person's local record, and the banner says memory.
func TestServeMemoryWritesNoLocalRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.Default()
	cfg.Storage.Driver = "memory"
	es, closeStore, err := openServeStore(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := es.(*agent.MemStore); !ok {
		t.Fatalf("serve opened %T, want memory", es)
	}
	ctx := context.Background()
	if rec, ok := es.(interface {
		CreateSession(context.Context, store.SessionRecord) error
	}); ok {
		_ = rec.CreateSession(ctx, store.SessionRecord{ID: "s-serve", Workspace: t.TempDir()})
	}
	if err := es.Append(agent.Event{SessionID: "s-serve", Seq: 1, Type: agent.EvSessionStarted, Actor: agent.ActorSystem, Trust: agent.Trusted}); err != nil {
		t.Fatal(err)
	}
	closeStore()
	if _, err := os.Stat(filepath.Join(home, ".abhed", "records")); !os.IsNotExist(err) {
		t.Fatalf("serve touched the local record: %v", err)
	}
	if l := serveStorageLabel(cfg); !strings.HasPrefix(l, "memory") {
		t.Fatalf("serve storage label = %q", l)
	}
}
