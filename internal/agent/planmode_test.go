package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

func defNames(defs []model.ToolDef) []string {
	var out []string
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

// exit_plan is offered only in plan mode, and a call to it in any other is
// refused as an unknown tool, with nothing proposed.
func TestExitPlanIsOnlyOfferedInPlanMode(t *testing.T) {
	for _, mode := range []policy.Mode{policy.ModeDefault, policy.ModeAcceptEdits, policy.ModeAuto, policy.ModeBypass, policy.ModePlan} {
		l, store, _ := harness(t, []scriptedTurn{
			{calls: []model.ToolCall{call("exit_plan", map[string]string{"plan": "1. change x"})}},
			{text: "ok"},
		}, mode, true)
		l.EnablePlanExit()
		if _, err := l.Run(context.Background(), "plan it"); err != nil {
			t.Fatal(err)
		}
		sent := defNames(l.Adapter.(*scriptedAdapter).gotRequests[0].Tools)
		evs, _ := store.Events("sess1")
		_, proposed := l.TakePlan()
		if mode == policy.ModePlan {
			if !slices.Contains(sent, "exit_plan") || !proposed || !hasEvent(evs, EvPlanProposed) {
				t.Fatalf("plan: offered %v, proposed %v", sent, proposed)
			}
			continue
		}
		if slices.Contains(sent, "exit_plan") || proposed || hasEvent(evs, EvPlanProposed) {
			t.Fatalf("%s: offered %v, proposed %v", mode, sent, proposed)
		}
		if got := lastToolResult(l); !strings.Contains(got, "Unknown tool") || strings.Contains(got, "exit_plan,") {
			t.Fatalf("%s: the model was told %q", mode, got)
		}
	}
}

// A proposed plan is recorded, ends the run without another model turn,
// changes no mode, and is handed over once.
func TestExitPlanProposesAndStops(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("exit_plan", map[string]string{"plan": "1. edit main.go\n2. run the tests"})}},
		{text: "never reached"},
	}, policy.ModePlan, true)
	l.EnablePlanExit()
	reason, err := l.Run(context.Background(), "plan it")
	if err != nil || reason != TermCompleted {
		t.Fatalf("%s %v", reason, err)
	}
	if n := len(l.Adapter.(*scriptedAdapter).gotRequests); n != 1 {
		t.Fatalf("the model was asked %d times; the plan should end the run", n)
	}
	if l.Policy.Mode != policy.ModePlan {
		t.Fatalf("the plan changed the mode to %s", l.Policy.Mode)
	}
	evs, _ := store.Events("sess1")
	var got []PlanProposed
	for _, e := range evs {
		if e.Type == EvPlanProposed {
			var p PlanProposed
			_ = json.Unmarshal(e.Payload, &p)
			if e.Actor != ActorAgent {
				t.Fatalf("plan credited to %s", e.Actor)
			}
			got = append(got, p)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0].Text, "run the tests") {
		t.Fatalf("recorded %+v", got)
	}
	if p, ok := l.TakePlan(); !ok || p.Text != got[0].Text {
		t.Fatalf("TakePlan %+v %v", p, ok)
	}
	if _, ok := l.TakePlan(); ok {
		t.Fatal("the plan was handed over twice")
	}
}
