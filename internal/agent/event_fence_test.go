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
type launchSeer struct{ seen *[3]string }

func (launchSeer) Name() string            { return "seer" }
func (launchSeer) Description() string     { return "sees its launch" }
func (launchSeer) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (launchSeer) Mutates() bool           { return false }
func (s launchSeer) Run(ctx context.Context, _ *tools.Session, _ json.RawMessage) tools.Result {
	l := sandbox.LaunchOf(ctx)
	s.seen[0], s.seen[1], s.seen[2] = CallIDOf(ctx), l.CallID, l.Session
	if l.Record != nil {
		_ = l.Record(string(EvProcessLaunched), map[string]any{"call_id": l.CallID})
	}
	return tools.Result{Content: "ok"}
}

// A tool's command carries its call id, as CallIDOf gives it, and the
// sandbox's launch event lands in the session's record.
func TestToolCallCarriesItsLaunch(t *testing.T) {
	var seen [3]string
	l, store, _ := harness(t, []scriptedTurn{{calls: []model.ToolCall{{ID: "call-7", Name: "seer", Args: json.RawMessage(`{}`)}}}, {text: "done"}},
		policy.ModeBypass, true)
	l.Tools.Add(launchSeer{seen: &seen})
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if seen != [3]string{"call-7", "call-7", "sess1"} {
		t.Fatalf("CallIDOf, the launch and its session were %q", seen)
	}
	evs, _ := store.Events("sess1")
	launched := payloads[map[string]any](evs, EvProcessLaunched)
	if len(launched) != 1 || launched[0]["call_id"] != "call-7" {
		t.Fatalf("process.launched: %v", launched)
	}
}

// A subagent's record, however deep, belongs to its top-level session,
// which keys what a sandbox keeps per session.
func TestRecorderRoot(t *testing.T) {
	top := NewRecorder(nil, "s", "")
	child := NewRecorder(nil, "c1", "s")
	child.root = top.Root()
	grand := NewRecorder(nil, "c2", "c1")
	grand.root = child.Root()
	if top.Root() != "s" || child.Root() != "s" || grand.Root() != "s" {
		t.Fatalf("roots %q %q %q", top.Root(), child.Root(), grand.Root())
	}
}
