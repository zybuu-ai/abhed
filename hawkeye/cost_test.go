package hawkeye

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func callEvent(seq int64, mc agent.ModelCall) agent.Event {
	b, _ := json.Marshal(mc)
	return agent.Event{Seq: seq, Type: agent.EvModelCall, Payload: b}
}

// A call marked unpriced is unpriced; a call from a record written before
// costing, with neither cost_usd nor unpriced, is not recorded, not unpriced.
func TestCostUnpricedVersusNotRecorded(t *testing.T) {
	usd := 0.01
	for _, c := range []struct {
		name  string
		calls []agent.ModelCall
		want  string
	}{
		{"priced", []agent.ModelCall{{Model: "gpt-4o", TokensIn: 10, CostUSD: &usd}}, "cost      $0.0100\n"},
		{"unpriced", []agent.ModelCall{{Model: "m", TokensIn: 10, Unpriced: true}}, "unpriced (no price for the model)"},
		{"old record", []agent.ModelCall{{Model: "gpt-4o", TokensIn: 10}}, "not recorded (the record predates costing)"},
		{"resumed old record", []agent.ModelCall{{Model: "gpt-4o", TokensIn: 10}, {Model: "gpt-4o", TokensIn: 10, CostUSD: &usd}},
			"$0.0100 (earlier calls not costed in the record)"},
	} {
		var evs []agent.Event
		for i, mc := range c.calls {
			evs = append(evs, callEvent(int64(i+1), mc))
		}
		r := Analyze("s", evs)
		if got := Text(r); !strings.Contains(got, c.want) {
			t.Errorf("%s: want %q in\n%s", c.name, c.want, got)
		}
		if c.name == "old record" && (r.Totals.Unpriced || !r.Totals.CostNotRecorded) {
			t.Errorf("old record totals %+v", r.Totals)
		}
	}
}
