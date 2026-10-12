package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// approvalRefusingStore refuses to record an approval, as a fenced session's
// store or a failing database does.
type approvalRefusingStore struct{ *MemStore }

func (s approvalRefusingStore) Append(ev Event) error {
	if ev.Type == EvActionApproved {
		return errors.New("the session is fenced on this node")
	}
	return s.MemStore.Append(ev)
}

// countTool counts its runs.
type countTool struct {
	name    string
	mutates bool
	runs    atomic.Int32
}

func (c *countTool) Name() string          { return c.name }
func (*countTool) Description() string     { return "counts" }
func (*countTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (c *countTool) Mutates() bool         { return c.mutates }
func (c *countTool) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	c.runs.Add(1)
	return tools.Result{Content: "ran"}
}

// A call whose approval cannot be recorded is never run, read-only or not:
// a process that took the session over would see no approval and could run
// it a second time. The run ends on the failed write.
func TestCallWithUnrecordedApprovalNeverRuns(t *testing.T) {
	dir := tempDir(t)
	sess, err := tools.NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	look, change := &countTool{name: "look"}, &countTool{name: "change", mutates: true}
	turns := []scriptedTurn{{calls: []model.ToolCall{
		{ID: "c1", Name: "look", Args: json.RawMessage(`{}`)},
		{ID: "c2", Name: "change", Args: json.RawMessage(`{}`)},
	}}, {text: "done"}}
	st := approvalRefusingStore{NewMemStore()}
	l := NewLoop(&scriptedAdapter{turns: turns}, tools.NewRegistry(look, change), policy.New(policy.ModeDefault),
		AutoApprove{Yes: true}, sess, NewRecorder(st, "sess1", ""), DefaultConfig())
	reason, err := l.Run(context.Background(), "go")
	if look.runs.Load() != 0 || change.runs.Load() != 0 {
		t.Fatalf("ran with no approval on record: look %d, change %d", look.runs.Load(), change.runs.Load())
	}
	if err == nil || reason != TermError {
		t.Fatalf("Run = %s, %v; want an error for the failed write", reason, err)
	}

	// A person's own call is refused the same way.
	ml := NewLoop(&scriptedAdapter{}, tools.NewRegistry(tools.Write{}), policy.New(policy.ModeAcceptEdits),
		AutoApprove{Yes: true}, sess, NewRecorder(approvalRefusingStore{NewMemStore()}, "sess2", ""), DefaultConfig())
	args, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, "x.txt"), "content": "x"})
	if _, refused, err := ml.ManualAuthorize("write", "u1", args); err == nil && refused == nil {
		t.Fatal("a person's call was authorized with its approval unrecorded")
	}
}
