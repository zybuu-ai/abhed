package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// A run records the plugins in force once per loop, before its first
// message, and a run with none records nothing.
func TestRunRecordsPluginsOnce(t *testing.T) {
	l, store := harnessIn(t, tempDir(t), nil, policy.ModeBypass, true)
	l.Config.Plugins = []PluginInForce{{Name: "acme-review", Version: "1.0.0", Digest: "ab12", Basis: "pin"}}
	for range 2 {
		if _, err := l.Run(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
	}
	evs, _ := store.Events("sess1")
	n, firstUser, at := 0, -1, -1
	var got PluginsLoaded
	for i, e := range evs {
		switch e.Type {
		case EvPluginLoaded:
			n++
			at = i
			_ = json.Unmarshal(e.Payload, &got)
		case EvUserMessage:
			if firstUser < 0 {
				firstUser = i
			}
		}
	}
	if n != 1 || at > firstUser || len(got.Plugins) != 1 || got.Plugins[0].Digest != "ab12" {
		t.Fatalf("%d plugin.loaded, at %d before the message at %d: %+v", n, at, firstUser, got)
	}

	l2, store2 := harnessIn(t, tempDir(t), nil, policy.ModeBypass, true)
	if _, err := l2.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	evs, _ = store2.Events("sess1")
	for _, e := range evs {
		if e.Type == EvPluginLoaded {
			t.Fatal("a run with no plugins recorded plugin.loaded")
		}
	}
}
