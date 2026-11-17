package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

type dottedTool struct{ ran *bool }

func (dottedTool) Name() string            { return "mcp__docs__search.pages" }
func (dottedTool) Description() string     { return "search the docs" }
func (dottedTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (dottedTool) Mutates() bool           { return false }
func (d dottedTool) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	*d.ran = true
	return tools.Result{Content: "found"}
}

// A call by a tool's wire name runs that tool, and the record names the tool
// by its real name, as policy and approval saw it.
func TestCallByWireNameRunsTheRealTool(t *testing.T) {
	ran := false
	wire := model.WireName(dottedTool{}.Name())
	if wire == (dottedTool{}).Name() {
		t.Fatal("the test needs a name the provider refuses")
	}
	sess, err := tools.NewSession(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	l := NewLoop(&scriptedAdapter{turns: []scriptedTurn{{calls: []model.ToolCall{{ID: "c1", Name: wire, Args: json.RawMessage(`{}`)}}}}},
		tools.NewRegistry(dottedTool{&ran}), policy.New(policy.ModeDefault), AutoApprove{Yes: true}, sess,
		NewRecorder(store, "s", ""), DefaultConfig())
	if _, err := l.Run(context.Background(), "search"); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("the call by the wire name did not run the tool")
	}
	evs, _ := store.Events("s")
	var named bool
	for _, e := range evs {
		if e.Type == EvActionRequested {
			raw, _ := json.Marshal(e.Payload)
			named = strings.Contains(string(raw), `"mcp__docs__search.pages"`) && !strings.Contains(string(raw), wire)
		}
	}
	if !named {
		t.Fatal("the record does not name the tool by its real name")
	}
}
