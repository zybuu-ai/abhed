package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// launchSeer reports the call a command would be launched for, and records
// a launch through what the sandbox is given.
type launchSeer struct{ seen *[2]string }

func (launchSeer) Name() string            { return "seer" }
func (launchSeer) Description() string     { return "sees its launch" }
func (launchSeer) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (launchSeer) Mutates() bool           { return false }
func (s launchSeer) Run(ctx context.Context, _ *tools.Session, _ json.RawMessage) tools.Result {
	l := sandbox.LaunchOf(ctx)
	s.seen[0], s.seen[1] = CallIDOf(ctx), l.CallID
	if l.Record != nil {
		_ = l.Record(string(EvProcessLaunched), map[string]any{"call_id": l.CallID})
	}
	return tools.Result{Content: "ok"}
}

// A tool's command carries its call id, as CallIDOf gives it, and the
// sandbox's launch event lands in the session's record.
func TestToolCallCarriesItsLaunch(t *testing.T) {
	var seen [2]string
	l, store, _ := harness(t, []scriptedTurn{{calls: []model.ToolCall{{ID: "call-7", Name: "seer", Args: json.RawMessage(`{}`)}}}, {text: "done"}},
		policy.ModeBypass, true)
	l.Tools.Add(launchSeer{seen: &seen})
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if seen != [2]string{"call-7", "call-7"} {
		t.Fatalf("CallIDOf and the launch saw %q", seen)
	}
	evs, _ := store.Events("sess1")
	launched := payloads[map[string]any](evs, EvProcessLaunched)
	if len(launched) != 1 || launched[0]["call_id"] != "call-7" {
		t.Fatalf("process.launched: %v", launched)
	}
}
