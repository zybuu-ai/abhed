package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

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

// sessionSeer stands in for bash, which every subagent role has, and keeps
// the session each launch of its calls names.
type sessionSeer struct {
	mu   *sync.Mutex
	seen map[string]string
}

func (sessionSeer) Name() string            { return "bash" }
func (sessionSeer) Description() string     { return "sees its launch" }
func (sessionSeer) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (sessionSeer) Mutates() bool           { return false }
func (s sessionSeer) Run(ctx context.Context, _ *tools.Session, _ json.RawMessage) tools.Result {
	l := sandbox.LaunchOf(ctx)
	s.mu.Lock()
	s.seen[l.CallID] = l.Session
	s.mu.Unlock()
	return tools.Result{Content: "seen"}
}

// Subagents' and grandchildren's commands launch as the root session's, which
// keys the sandbox's state for it, such as its egress proxy.
func TestSubagentLaunchesAsItsRootSession(t *testing.T) {
	seer := sessionSeer{mu: &sync.Mutex{}, seen: map[string]string{}}
	store := NewMemStore()
	l, _, f := taskTree(t, &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{taskCall("t1", "outer")}},
		{calls: []model.ToolCall{bashCall("s-child", "echo child")}},
		{calls: []model.ToolCall{taskCall("t2", "inner")}},
		{calls: []model.ToolCall{bashCall("s-grand", "echo grand")}},
		{text: "grandchild done"},
		{text: "child done"},
		{text: "done"},
	}}, AutoApprove{Yes: true}, store, store, true)
	f.Tools.Add(seer)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := l.Run(ctx, "go"); err != nil {
		t.Fatal(err)
	}
	seer.mu.Lock()
	defer seer.mu.Unlock()
	if seer.seen["s-child"] != "parent" || seer.seen["s-grand"] != "parent" {
		t.Fatalf("launches named sessions %v, want parent for both", seer.seen)
	}
}
