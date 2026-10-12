package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// approvalRefusingStore refuses approvals whose payload holds only, and every
// write once failAll is set, as a fenced store or failing database does.
type approvalRefusingStore struct {
	*MemStore
	only    string
	failAll *atomic.Bool
}

func (s approvalRefusingStore) Append(ev Event) error {
	if s.failAll != nil && s.failAll.Load() ||
		ev.Type == EvActionApproved && strings.Contains(string(ev.Payload), s.only) {
		return errors.New("the session is fenced on this node")
	}
	return s.MemStore.Append(ev)
}

// countTool counts its runs.
type countTool struct {
	name    string
	mutates bool
	runs    atomic.Int32
	onRun   func()
}

func (c *countTool) Name() string          { return c.name }
func (*countTool) Description() string     { return "counts" }
func (*countTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (c *countTool) Mutates() bool         { return c.mutates }
func (c *countTool) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	c.runs.Add(1)
	if c.onRun != nil {
		c.onRun()
	}
	return tools.Result{Content: "ran"}
}

// A call whose approval cannot be recorded is never run, read-only or not, since a
// process taking the session over could run it again; the run ends on the failed write.
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
	st := approvalRefusingStore{MemStore: NewMemStore()}
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
		AutoApprove{Yes: true}, sess, NewRecorder(approvalRefusingStore{MemStore: NewMemStore()}, "sess2", ""), DefaultConfig())
	args, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, "x.txt"), "content": "x"})
	if _, refused, err := ml.ManualAuthorize("write", "u1", args); err == nil && refused == nil {
		t.Fatal("a person's call was authorized with its approval unrecorded")
	}
}

// Each guard holds alone: a later failed approval, a write failing during the
// read-only batch, and an unrecorded pipeline-step approval each stop the run.
func TestUnrecordedApprovalEachGuard(t *testing.T) {
	sess, err := tools.NewSession(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	run := func(st approvalRefusingStore, look, change *countTool) {
		t.Helper()
		turns := []scriptedTurn{{calls: []model.ToolCall{
			{ID: "c1", Name: "look", Args: json.RawMessage(`{}`)},
			{ID: "c2", Name: "change", Args: json.RawMessage(`{}`)},
		}}, {text: "done"}}
		l := NewLoop(&scriptedAdapter{turns: turns}, tools.NewRegistry(look, change), policy.New(policy.ModeDefault),
			AutoApprove{Yes: true}, sess, NewRecorder(st, "s", ""), DefaultConfig())
		if reason, err := l.Run(context.Background(), "go"); err == nil || reason != TermError {
			t.Fatalf("Run = %s, %v; want an error for the failed write", reason, err)
		}
	}

	// Only c2's approval fails: c1's is on record, but the record is broken.
	look, change := &countTool{name: "look"}, &countTool{name: "change", mutates: true}
	run(approvalRefusingStore{MemStore: NewMemStore(), only: `"call_id":"c2"`}, look, change)
	if look.runs.Load() != 0 || change.runs.Load() != 0 {
		t.Errorf("ran after a later approval failed: look %d, change %d", look.runs.Load(), change.runs.Load())
	}

	// Both approvals recorded; a write fails while the read-only call runs.
	var failAll atomic.Bool
	st := approvalRefusingStore{MemStore: NewMemStore(), only: "\x00", failAll: &failAll}
	look, change = &countTool{name: "look"}, &countTool{name: "change", mutates: true}
	var l *Loop
	look.onRun = func() {
		failAll.Store(true)
		l.record(EvUserMessage, ActorUser, Message{Text: "x"}) // as a concurrent step's event
	}
	turns := []scriptedTurn{{calls: []model.ToolCall{
		{ID: "c1", Name: "look", Args: json.RawMessage(`{}`)},
		{ID: "c2", Name: "change", Args: json.RawMessage(`{}`)},
	}}, {text: "done"}}
	l = NewLoop(&scriptedAdapter{turns: turns}, tools.NewRegistry(look, change), policy.New(policy.ModeDefault),
		AutoApprove{Yes: true}, sess, NewRecorder(st, "s3", ""), DefaultConfig())
	_, _ = l.Run(context.Background(), "go")
	if look.runs.Load() != 1 || change.runs.Load() != 0 {
		t.Errorf("a mutating call ran after a write failed: look %d, change %d", look.runs.Load(), change.runs.Load())
	}

	// A pipeline step whose approval is not recorded is not run.
	step := &countTool{name: "change", mutates: true}
	pl := NewLoop(&scriptedAdapter{}, tools.NewRegistry(step), policy.New(policy.ModeDefault),
		AutoApprove{Yes: true}, sess, NewRecorder(approvalRefusingStore{MemStore: NewMemStore()}, "s4", ""), DefaultConfig())
	res, _ := (&Steps{loop: pl, via: "skill a pipeline"}).Run(context.Background(), "change", json.RawMessage(`{}`), 0)
	if step.runs.Load() != 0 || !res.IsError {
		t.Errorf("a pipeline step ran with its approval unrecorded: %d runs, %+v", step.runs.Load(), res)
	}
}
