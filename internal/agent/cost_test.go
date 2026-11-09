package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

func costHarness(t *testing.T, name string, turns []scriptedTurn) (*Loop, *MemStore) {
	t.Helper()
	l, store, _ := harness(t, turns, policy.ModeBypass, true)
	l.Adapter = namedAdapter{scriptedAdapter: l.Adapter.(*scriptedAdapter), name: name, window: 100000}
	l.Budget = NewBudget(0, 0, false)
	return l, store
}

func modelCalls(t *testing.T, store *MemStore) []ModelCall {
	t.Helper()
	evs, _ := store.Events("sess1")
	var out []ModelCall
	for _, e := range evs {
		if e.Type == EvModelCall {
			var c ModelCall
			_ = json.Unmarshal(e.Payload, &c)
			out = append(out, c)
		}
	}
	return out
}

func budgetStops(t *testing.T, store *MemStore) []BudgetStopped {
	t.Helper()
	evs, _ := store.Events("sess1")
	var out []BudgetStopped
	for _, e := range evs {
		if e.Type == EvBudgetStopped {
			var b BudgetStopped
			_ = json.Unmarshal(e.Payload, &b)
			out = append(out, b)
		}
	}
	return out
}

var turnUsage = &model.Usage{InputTokens: 1000, CachedInputTokens: 400, OutputTokens: 200}

// Each call to a priced model records its cost, and the session's end and
// usage carry the total.
func TestCostIsRecordedPerCallAndSession(t *testing.T) {
	l, store := costHarness(t, "gpt-4o", []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "true"})}, usage: turnUsage},
		{text: "done", usage: turnUsage},
	})
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	per := (600*2.50 + 400*1.25 + 200*10.00) / 1e6
	calls := modelCalls(t, store)
	if len(calls) != 2 {
		t.Fatalf("%d model calls", len(calls))
	}
	for _, c := range calls {
		if c.CostUSD == nil || !near(*c.CostUSD, per) {
			t.Fatalf("call cost %v, want %v", c.CostUSD, per)
		}
	}
	if u := l.Usage(); !near(u.CostUSD, 2*per) || u.Unpriced {
		t.Fatalf("usage %+v", u)
	}
	end, ok := LastEnd(sessEvents(t, store))
	if !ok || !near(end.CostUSD, 2*per) || end.Unpriced {
		t.Fatalf("end %+v", end)
	}
	if !near(l.Budget.SpentUSD(), 2*per) {
		t.Fatalf("budget spent %v", l.Budget.SpentUSD())
	}
}

// A model with no price records no cost and marks the session unpriced.
func TestUnpricedModelIsNotGuessed(t *testing.T) {
	l, store := costHarness(t, "gemma4:26b", []scriptedTurn{{text: "done", usage: turnUsage}})
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if c := modelCalls(t, store); len(c) != 1 || c[0].CostUSD != nil {
		t.Fatalf("calls %+v", c)
	}
	if u := l.Usage(); u.CostUSD != 0 || !u.Unpriced {
		t.Fatalf("usage %+v", u)
	}
}

// A configured price prices a model the list does not have.
func TestConfiguredPrice(t *testing.T) {
	l, store := costHarness(t, "gemma4:26b", []scriptedTurn{{text: "done", usage: turnUsage}})
	l.Budget.Prices = map[string]model.Price{"gemma4:26b": {Input: 1, Output: 1}}
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if c := modelCalls(t, store); len(c) != 1 || c[0].CostUSD == nil || !near(*c[0].CostUSD, 1200.0/1e6) {
		t.Fatalf("calls %+v", c)
	}
}

// Past the dollar cap the run stops cleanly at the next turn, with exit 3,
// a recorded reason and a message.
func TestDollarBudgetStopsTheRun(t *testing.T) {
	l, store := costHarness(t, "gpt-4o", []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "true"})}, usage: &model.Usage{InputTokens: 100000, OutputTokens: 1000}},
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "true"})}, usage: turnUsage},
		{text: "done", usage: turnUsage},
	})
	l.Budget.MaxUSD = 0.10 // the first call costs $0.26
	reason, err := l.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if reason != TermMaxBudget || reason.ExitCode() != 3 {
		t.Fatalf("reason %s", reason)
	}
	if n := len(modelCalls(t, store)); n != 1 {
		t.Fatalf("%d calls made past the cap", n)
	}
	stops := budgetStops(t, store)
	if len(stops) != 1 || stops[0].Reason != "usd" || stops[0].MaxUSD != 0.10 || stops[0].Message == "" {
		t.Fatalf("stops %+v", stops)
	}
}

// A dollar cap with a model that has no price fails closed: no call is made.
func TestDollarBudgetRefusesAnUnpricedModel(t *testing.T) {
	l, store := costHarness(t, "gemma4:26b", []scriptedTurn{{text: "done", usage: turnUsage}})
	l.Budget.MaxUSD = 5
	reason, err := l.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if reason != TermMaxBudget {
		t.Fatalf("reason %s", reason)
	}
	if n := len(modelCalls(t, store)); n != 0 {
		t.Fatalf("%d calls made to an unpriced model under a dollar cap", n)
	}
	if stops := budgetStops(t, store); len(stops) != 1 || stops[0].Reason != "unpriced" || stops[0].Model != "gemma4:26b" {
		t.Fatalf("stops %+v", stops)
	}
}

// A subagent's spawn is refused once the dollar cap is reached, and a
// continued session starts from the dollars its record says it spent.
func TestDollarBudgetSharedAndCarried(t *testing.T) {
	b := NewBudget(0, 0, false)
	b.MaxUSD = 1
	b.SpendUSD(1.5)
	if err := b.TryReserveSubagent(); err == nil {
		t.Fatal("a spawn past the dollar cap must be refused")
	}
	c := NewBudget(0, 0, false)
	c.Carry(0, 0, 0.75)
	if !near(c.SpentUSD(), 0.75) {
		t.Fatalf("carried %v", c.SpentUSD())
	}
	st := NewMemStore()
	p := NewRecorder(st, "p", "")
	_, _ = p.Record(EvSubagentReturn, ActorAgent, Trusted, map[string]any{"session": "a", "tokens_in": 1, "cost_usd": 0.25})
	_, _ = p.Record(EvSessionEnded, ActorSystem, Trusted, SessionEnded{Reason: TermCompleted, CostUSD: 0.5})
	evs, _ := st.Events("p")
	if _, _, usd := CarriedSpend(evs); !near(usd, 0.75) {
		t.Fatalf("carried spend %v", usd)
	}
}

func near(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }

func sessEvents(t *testing.T, s *MemStore) []Event {
	t.Helper()
	evs, err := s.Events("sess1")
	if err != nil {
		t.Fatal(err)
	}
	return evs
}
