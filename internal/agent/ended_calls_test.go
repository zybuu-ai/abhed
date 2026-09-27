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

// stopAtApproval interrupts the run while a call waits for an answer, as
// Ctrl-C at the approval prompt does.
type stopAtApproval struct{ cancel context.CancelFunc }

func (s stopAtApproval) Approve(ctx context.Context, _ string, _ json.RawMessage, _ policy.Result) (bool, error) {
	s.cancel()
	<-ctx.Done()
	return false, ctx.Err()
}

// A turn interrupted at the approval of one of several calls still answers
// every call, so the next request the conversation sends is well formed.
func TestInterruptedTurnAnswersEveryCall(t *testing.T) {
	dir := tempDir(t)
	sess, err := tools.NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	calls := []model.ToolCall{
		{ID: "c1", Name: "glob", Args: json.RawMessage(`{"pattern":"*"}`)},
		{ID: "c2", Name: "write", Args: json.RawMessage(`{"path":"` + dir + `/a.txt","content":"a"}`)},
		{ID: "c3", Name: "write", Args: json.RawMessage(`{"path":"` + dir + `/b.txt","content":"b"}`)},
	}
	ad := &scriptedAdapter{turns: []scriptedTurn{{calls: calls}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemStore()
	l := NewLoop(ad, tools.NewRegistry(tools.Glob{}, tools.Write{}), policy.New(policy.ModeDefault),
		stopAtApproval{cancel}, sess, NewRecorder(store, "s1", ""), DefaultConfig())
	if reason, err := l.Run(ctx, "go"); err != nil || reason != TermUserInterrupt {
		t.Fatalf("first run = %s, %v", reason, err)
	}
	if _, err := l.Run(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	req := ad.gotRequests[len(ad.gotRequests)-1]
	answered := map[string]bool{}
	for _, m := range req.Messages {
		if m.Role == model.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	for _, m := range req.Messages {
		for _, c := range m.ToolCalls {
			if !answered[c.ID] {
				t.Errorf("call %s is sent with no result", c.ID)
			}
		}
		if m.ToolCallID == "c1" && !strings.HasPrefix(m.Content, "Not run: the turn ended") {
			t.Errorf("the call that never ran is answered %q", m.Content)
		}
	}

	// The conversation rebuilt from the record, as /resume and the server's
	// continuation do, answers every call too and keeps the later task.
	events, _ := store.Events("s1")
	msgs, err := Fork(events, 0)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, calledAt, next := map[string]string{}, 0, false
	for i, m := range msgs {
		if m.Role == model.RoleTool {
			rebuilt[m.ToolCallID] = m.Content
		}
		if len(m.ToolCalls) > 0 {
			calledAt = i
		}
		next = next || m.Content == "next"
	}
	for _, c := range msgs[calledAt].ToolCalls {
		if !strings.HasPrefix(rebuilt[c.ID], "Not run") {
			t.Errorf("rebuilt call %s is answered %q", c.ID, rebuilt[c.ID])
		}
	}
	if !next {
		t.Error("the task after the interrupted turn was dropped from the rebuild")
	}
	// A call that never ran has an answer in the record, marked not run.
	answeredInRecord := false
	for _, ev := range events {
		var o Observation
		if ev.Type == EvObservation && json.Unmarshal(ev.Payload, &o) == nil && o.CallID == "c1" {
			if !o.NotRun {
				t.Error("the call that never ran is recorded as if it ran")
			}
			answeredInRecord = true
		}
	}
	if !answeredInRecord {
		t.Error("the record holds no answer for the call that never ran")
	}
	// Only a requested call is answered in the record: c3 never was.
	requested := map[string]bool{}
	for _, ev := range events {
		var a ActionRequested
		if ev.Type == EvActionRequested && json.Unmarshal(ev.Payload, &a) == nil {
			requested[a.CallID] = true
		}
	}
	for _, ev := range events {
		var o Observation
		if ev.Type == EvObservation && json.Unmarshal(ev.Payload, &o) == nil && !requested[o.CallID] {
			t.Errorf("an observation for %s, which was never requested", o.CallID)
		}
	}
}
