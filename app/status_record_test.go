package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/store/local"
)

// /status and the statusline name the store the session writes to: the
// local record when it is in use, memory only when it is not.
func TestStatusNamesTheLocalRecord(t *testing.T) {
	env, surface, _ := recordedEnv(t, config.Default(), "default")
	if m := env.st.statusModel("default"); m.Record != ui.RecordMemory {
		t.Fatalf("memory store: record %q", m.Record)
	}
	rec, err := local.Open(local.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	env.st.store = rec
	m := env.st.statusModel("default")
	if b, _ := json.Marshal(m); m.Record != ui.RecordLocal || !strings.Contains(string(b), `"record":"local"`) {
		t.Fatalf("local record: %s", b)
	}
	if _, err := slashStatus(context.Background(), env, nil); err != nil {
		t.Fatal(err)
	}
	if out := surface.text(); !strings.Contains(out, "local record, chained") || strings.Contains(out, "memory only") {
		t.Fatalf("status:\n%s", out)
	}
}

// /status says the turn limit is per message unless the managed
// configuration sets it, when it bounds the whole conversation.
func TestStatusTurnLimitFollowsTheManagedConfiguration(t *testing.T) {
	own := config.Default()
	own.Limits.MaxTurns = 30
	managed := own
	managed.Managed, managed.ManagedKeys = true, []string{"limits.max_turns"}
	for _, c := range []struct {
		cfg       config.Config
		want, not string
	}{{own, "30 for each message", "whole conversation"}, {managed, "30 for the whole conversation", "for each message"}} {
		env, surface, _ := recordedEnv(t, c.cfg, "default")
		if _, err := slashStatus(context.Background(), env, nil); err != nil {
			t.Fatal(err)
		}
		if out := surface.text(); !strings.Contains(out, c.want) || strings.Contains(out, c.not) {
			t.Fatalf("status:\n%s", out)
		}
	}
}
