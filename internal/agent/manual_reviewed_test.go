package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/monitor"
)

func judge(d policy.Decision, err error) monitor.Func {
	return func(context.Context, monitor.Case) (monitor.Verdict, error) {
		if err != nil {
			return monitor.Verdict{}, err
		}
		return monitor.Verdict{Decision: d, Code: "unknown-program", Rationale: "why"}, nil
	}
}

// The supervisor's review of a person's own action: a deny refuses with its
// verdict recorded; an unavailable judge under fail-closed refuses; an ask
// goes to the person, and only their approval lets it through.
func TestManualAuthorizeReviewed(t *testing.T) {
	args := bashArgs("go test ./...")
	for _, c := range []struct {
		name  string
		guard *monitor.Guard
		ask   ManualAsk
		ok    bool
		step  string
	}{
		{"deny", &monitor.Guard{Monitor: judge(policy.Deny, nil)}, nil, false, "monitor"},
		{"unavailable, fail-closed", &monitor.Guard{Monitor: judge("", errors.New("down")), OnUnavailable: policy.Deny}, nil, false, "monitor"},
		{"ask, approved", &monitor.Guard{Monitor: judge(policy.Ask, nil)}, func(context.Context, policy.Result) (bool, error) { return true, nil }, true, "monitor"},
		{"ask, declined", &monitor.Guard{Monitor: judge(policy.Ask, nil)}, func(context.Context, policy.Result) (bool, error) { return false, nil }, false, "monitor"},
		{"ask, nobody to ask", &monitor.Guard{Monitor: judge(policy.Ask, nil)}, nil, false, "unanswered"},
		{"ask, no answer", &monitor.Guard{Monitor: judge(policy.Ask, nil)}, func(context.Context, policy.Result) (bool, error) { return false, errors.New("closed") }, false, "unanswered"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, store := manualLoop(t)
			l.Monitor = c.guard
			tool, refused, err := l.ManualAuthorizeReviewed(context.Background(), "bash", "u1", args, c.ask)
			if err != nil || (tool != nil) != c.ok || (refused == nil) != c.ok {
				t.Fatalf("tool %v refused %v err %v", tool, refused, err)
			}
			ds := decisionsOf(t, store, "u1")
			var p map[string]string
			if len(ds) == 1 {
				_ = json.Unmarshal(ds[0].Payload, &p)
			}
			if len(ds) != 1 || p["step"] != c.step {
				t.Fatalf("decision %+v %v", ds, p)
			}
			evs, _ := store.Events("s-manual")
			verdict := false
			for _, e := range evs {
				verdict = verdict || e.Type == EvMonitorVerdict
			}
			if !verdict {
				t.Fatal("the verdict is not in the record")
			}
		})
	}
}
