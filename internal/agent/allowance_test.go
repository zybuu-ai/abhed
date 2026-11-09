package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
)

// A limit kept outside the session is asked before every turn, the first
// included: spent, the run makes no call and says which limit stopped it.
func TestAllowanceStopsBeforeTheFirstCall(t *testing.T) {
	l, store := costHarness(t, "gpt-4o", []scriptedTurn{{text: "done", usage: turnUsage}})
	l.Budget.Allowance = func(context.Context) Allowance {
		return Allowance{Stop: true, Scope: "user bob, monthly", Message: "stopped: the monthly budget of user bob is spent"}
	}
	reason, err := l.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if reason != TermMaxBudget {
		t.Fatalf("reason %s", reason)
	}
	if c := modelCalls(t, store); len(c) != 0 {
		t.Fatalf("%d calls past a spent allowance", len(c))
	}
	stops := budgetStops(t, store)
	if len(stops) != 1 || stops[0].Reason != "allowance" || stops[0].Scope != "user bob, monthly" ||
		stops[0].Message != "stopped: the monthly budget of user bob is spent" {
		t.Fatalf("stops %+v", stops)
	}
}

// Asked again at each boundary: an allowance spent mid-run ends it at the
// next turn, never mid-turn.
func TestAllowanceIsAskedAtEveryTurnBoundary(t *testing.T) {
	l, store := costHarness(t, "gpt-4o", []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "true"})}, usage: turnUsage},
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "true"})}, usage: turnUsage},
		{text: "done", usage: turnUsage},
	})
	var asked atomic.Int32
	l.Budget.Allowance = func(context.Context) Allowance {
		return Allowance{Stop: asked.Add(1) > 1, Scope: "workspace pay, daily"}
	}
	if reason, _ := l.Run(context.Background(), "go"); reason != TermMaxBudget {
		t.Fatalf("reason %s", reason)
	}
	if c := modelCalls(t, store); len(c) != 1 {
		t.Fatalf("%d calls, want 1", len(c))
	}
	if stops := budgetStops(t, store); len(stops) != 1 || stops[0].Message == "" {
		t.Fatalf("stops %+v", stops)
	}
}

// Under an outside dollar limit a model with no price is refused, as a
// dollar cap of the session's own refuses it; a priced one runs.
func TestAllowanceInDollarsRefusesAnUnpricedModel(t *testing.T) {
	l, store := costHarness(t, "gemma4:26b", []scriptedTurn{{text: "done", usage: turnUsage}})
	l.Budget.Allowance = func(context.Context) Allowance { return Allowance{USD: true, Scope: "group eng, weekly"} }
	if reason, _ := l.Run(context.Background(), "go"); reason != TermMaxBudget {
		t.Fatalf("reason %s", reason)
	}
	if c := modelCalls(t, store); len(c) != 0 {
		t.Fatalf("%d calls to an unpriced model", len(c))
	}
	if stops := budgetStops(t, store); len(stops) != 1 || stops[0].Reason != "unpriced" || stops[0].Scope != "group eng, weekly" {
		t.Fatalf("stops %+v", stops)
	}

	l, store = costHarness(t, "gpt-4o", []scriptedTurn{{text: "done", usage: turnUsage}})
	l.Budget.Allowance = func(context.Context) Allowance { return Allowance{USD: true, Scope: "group eng, weekly"} }
	if reason, _ := l.Run(context.Background(), "go"); reason == TermMaxBudget {
		t.Fatal("a priced model was stopped")
	}
	if c := modelCalls(t, store); len(c) != 1 {
		t.Fatalf("%d calls", len(c))
	}
	// The dollar limit stays known to the budget, for every path that calls
	// a model, not only the turn boundary.
	if !l.Budget.USDLimited() {
		t.Fatal("the budget does not know a dollar limit applies")
	}
}
