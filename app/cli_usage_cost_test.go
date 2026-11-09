package app

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// /usage says a call from a record written before costing is not recorded,
// and only a call marked so is unpriced.
func TestTurnCostsUnpricedVersusNotRecorded(t *testing.T) {
	usd := 0.5
	var evs []agent.Event
	for i, mc := range []agent.ModelCall{
		{Turn: 1, Model: "gpt-4o", TokensIn: 10},
		{Turn: 2, Model: "m", TokensIn: 10, Unpriced: true},
		{Turn: 3, Model: "gpt-4o", TokensIn: 10, CostUSD: &usd},
	} {
		b, _ := json.Marshal(mc)
		evs = append(evs, agent.Event{Seq: int64(i + 1), Type: agent.EvModelCall, Payload: b})
	}
	rows := turnCosts(evs, 10)
	got := []string{rows[1][5], rows[2][5], rows[3][5]}
	if !slices.Equal(got, []string{"not recorded", "cost unpriced", "$0.5000"}) {
		t.Fatalf("costs %q", got)
	}
}

// A run's line is unpriced only when a call in that run was.
func TestUsageSinceUnpricedIsTheRuns(t *testing.T) {
	before := agent.Usage{Unpriced: true, UnpricedCalls: 1, CostUSD: 1}
	after := agent.Usage{Unpriced: true, UnpricedCalls: 1, CostUSD: 1.5}
	if u := usageSince(before, after); u.Unpriced || u.CostUSD != 0.5 {
		t.Fatalf("a priced run after an unpriced one: %+v", u)
	}
	after.UnpricedCalls = 2
	if u := usageSince(before, after); !u.Unpriced {
		t.Fatalf("a run with an unpriced call: %+v", u)
	}
}

// §3.4: after a replay of calls recorded before costing, the session's cost
// is sent as partial and never as the spec's session cost.
func TestACPSessionCostPartialAfterOldReplay(t *testing.T) {
	c := &acpConn{}
	s := &acpSession{id: "s1", always: map[string]string{}}
	old, _ := json.Marshal(agent.ModelCall{Model: "gpt-4o", TokensIn: 10, ContextWindow: 1000})
	c.updates(s, agent.Event{Seq: 1, Type: agent.EvModelCall, Payload: old}, true)
	usd := 0.25
	now, _ := json.Marshal(agent.ModelCall{Model: "gpt-4o", TokensIn: 10, ContextWindow: 1000, CostUSD: &usd})
	ups := c.updates(s, agent.Event{Seq: 2, Type: agent.EvModelCall, Payload: now}, false)
	if len(ups) != 1 {
		t.Fatalf("updates %v", ups)
	}
	m := metaOf(ups[0])
	if ups[0]["cost"] != nil || m["costPartial"] != true || m["sessionCostUsd"] != 0.25 {
		t.Fatalf("usage_update %v", ups[0])
	}
}

// §3.4: replayed calls count in the session's tokens as in its cost, and an
// editor summary's cost is the session's too.
func TestACPReplayCountsTokensAndEditorCost(t *testing.T) {
	c := &acpConn{}
	s := &acpSession{id: "s1", always: map[string]string{}}
	usd := 0.25
	old, _ := json.Marshal(agent.ModelCall{Model: "gpt-4o", TokensIn: 100, TokensOut: 5, ContextWindow: 1000, CostUSD: &usd})
	c.updates(s, agent.Event{Seq: 1, Type: agent.EvModelCall, Payload: old}, true)
	sum, _ := json.Marshal(agent.CompletionSummary{CostUSD: 0.5})
	c.updates(s, agent.Event{Seq: 2, Type: agent.EvCompletionSummary, Payload: sum}, true)
	ups := c.updates(s, agent.Event{Seq: 3, Type: agent.EvModelCall, Payload: old}, false)
	m := metaOf(ups[0])
	if m["sessionTotalIn"] != 200 || m["sessionCostUsd"] != 1.0 {
		t.Fatalf("usage_update %v", ups[0])
	}
}
