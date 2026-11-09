package agent

import (
	"context"
	"strings"
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

// A run interrupted while its allowance is asked ends as interrupted, not as
// over budget, and makes no call.
func TestAllowanceInterruptedEndsAsStopped(t *testing.T) {
	l, store := costHarness(t, "gpt-4o", []scriptedTurn{{text: "done", usage: turnUsage}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.Budget.Allowance = func(ctx context.Context) Allowance {
		cancel()
		<-ctx.Done()
		return Allowance{}
	}
	reason, _ := l.Run(ctx, "go")
	if reason == TermMaxBudget || reason != terminalForCancel(ctx) {
		t.Fatalf("reason %s", reason)
	}
	if len(budgetStops(t, store)) != 0 || len(modelCalls(t, store)) != 0 {
		t.Fatal("an interrupt was recorded as a budget stop, or a call was made")
	}
}

// The gate every call between turns passes (a suggestion, the critic, a
// compaction, a pipeline step, a fallback) reads the allowance's last
// answer: after a stop no call goes out, and under its dollar limit an
// unpriced model is refused; an answer that allows again lifts the stop.
func TestAllowanceHoldsCallsBetweenTurns(t *testing.T) {
	l, _ := costHarness(t, "gpt-4o", []scriptedTurn{{text: "done", usage: turnUsage}})
	stop := true
	l.Budget.Allowance = func(context.Context) Allowance {
		return Allowance{Stop: stop, USD: true, Scope: "user bob, daily", Message: "stopped: the daily budget of user bob is spent"}
	}
	if reason, _ := l.Run(context.Background(), "go"); reason != TermMaxBudget {
		t.Fatalf("reason %s", reason)
	}
	if err := l.Budget.Refuse("gpt-4o"); err == nil || err.Error() != "the daily budget of user bob is spent" {
		t.Fatalf("a call after the stop: %v", err)
	}
	stop = false
	l2, _ := costHarness(t, "gpt-4o", []scriptedTurn{{text: "done", usage: turnUsage}})
	l2.Budget = l.Budget
	if _, err := l2.Run(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if err := l.Budget.Refuse("gpt-4o"); err != nil {
		t.Fatalf("an allowing answer did not lift the stop: %v", err)
	}
	if err := l.Budget.Refuse("gemma4:26b"); err == nil || !strings.Contains(err.Error(), "a dollar budget applies and gemma4:26b has no price") {
		t.Fatalf("an unpriced side model under the dollar limit: %v", err)
	}
}
