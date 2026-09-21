package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// askCounter approves everything and counts how often it was asked.
type askCounter struct{ asked int }

func (a *askCounter) Approve(context.Context, string, json.RawMessage, policy.Result) (bool, error) {
	a.asked++
	return true, nil
}

// A relative path can never succeed, so nobody is asked to approve it, and the
// model still gets the correction.
func TestDoomedCallIsNotPutToAPerson(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("write", map[string]string{"path": "new.txt", "content": "x"})}},
		{text: "ok"},
	}, policy.ModeDefault, true)
	counter := &askCounter{}
	l.Approver = counter

	l.Run(context.Background(), "write a file")

	if counter.asked != 0 {
		t.Fatalf("asked %d times about a call that could not succeed", counter.asked)
	}
	evs, _ := store.Events("sess1")
	for _, e := range evs {
		if e.Type != EvActionRequested {
			continue
		}
		var p ActionRequested
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.RequiresApproval {
			t.Fatal("the record says approval was required for a call that was never put to anyone")
		}
	}
	adapter := l.Adapter.(*scriptedAdapter)
	last := adapter.gotRequests[len(adapter.gotRequests)-1]
	found := false
	for _, m := range last.Messages {
		if m.Role == model.RoleTool && strings.Contains(m.Content, "must be absolute") {
			found = true
		}
	}
	if !found {
		t.Fatal("the model was not told how to correct the path")
	}
}

// A deny rule still reads as a denial, not as a failed precheck.
func TestPrecheckDoesNotMaskADeny(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("write", map[string]string{"path": "new.txt", "content": "x"})}},
		{text: "ok"},
	}, policy.ModePlan, true)

	l.Run(context.Background(), "write a file")

	evs, _ := store.Events("sess1")
	if !hasEvent(evs, EvActionDenied) {
		t.Fatal("plan mode must still record the write as denied")
	}
}
