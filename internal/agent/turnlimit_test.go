package agent

import (
	"context"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// busy is n turns that each call a tool, so a run only ends at its limit.
func busy(n int) []scriptedTurn {
	out := make([]scriptedTurn, n)
	for i := range out {
		out[i] = scriptedTurn{calls: []model.ToolCall{call("glob", map[string]string{"pattern": "*.none"})}}
	}
	return out
}

// Per message, each message a person sends gets its own turns; for the
// whole conversation, as a managed limit is, the second message gets none.
func TestTurnsPerMessage(t *testing.T) {
	for _, c := range []struct {
		name      string
		perMsg    int
		secondGot int
	}{
		{"per message", 2, 2},
		{"whole conversation", 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, _, _ := harness(t, busy(10), policy.ModeDefault, true)
			l.Config.MaxTurns, l.Config.TurnsPerMessage = 2, c.perMsg
			adapter := l.Adapter.(*scriptedAdapter)
			if reason, err := l.Run(context.Background(), "first"); err != nil || reason != TermMaxTurns {
				t.Fatalf("first: %s %v", reason, err)
			}
			if len(adapter.gotRequests) != 2 {
				t.Fatalf("first message used %d turns", len(adapter.gotRequests))
			}
			if reason, err := l.Run(context.Background(), "second"); err != nil || reason != TermMaxTurns {
				t.Fatalf("second: %s %v", reason, err)
			}
			if got := len(adapter.gotRequests) - 2; got != c.secondGot {
				t.Fatalf("second message used %d turns, want %d", got, c.secondGot)
			}
		})
	}
}
