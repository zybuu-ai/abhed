package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// A run stopped at its turn limit goes on with ContinueAfterLimit: the
// continuation is recorded first, gives ContinueTurns more turns, and is
// bounded by MaxContinues; with none allowed it is refused.
func TestContinueAfterLimit(t *testing.T) {
	l, store, _ := harness(t, varied(20), policy.ModeDefault, true)
	l.Config.MaxTurns, l.Config.ContinueTurns, l.Config.MaxContinues = 2, 3, 2
	adapter := l.Adapter.(*scriptedAdapter)
	ctx := context.Background()
	if _, err := l.ContinueAfterLimit(ctx, "tui"); !errors.Is(err, ErrNothingToContinue) {
		t.Fatalf("before any run: %v", err)
	}
	if reason, err := l.Run(ctx, "go"); err != nil || reason != TermMaxTurns {
		t.Fatalf("run: %s %v", reason, err)
	}
	for i := 1; i <= 2; i++ {
		before := len(adapter.gotRequests)
		reason, err := l.ContinueAfterLimit(ctx, "tui")
		if err != nil || reason != TermMaxTurns {
			t.Fatalf("continue %d: %s %v", i, reason, err)
		}
		if got := len(adapter.gotRequests) - before; got != 3 {
			t.Fatalf("continue %d used %d turns, want 3", i, got)
		}
	}
	if _, err := l.ContinueAfterLimit(ctx, "tui"); !errors.Is(err, ErrContinueRefused) {
		t.Fatalf("past the bound: %v", err)
	}
	events, _ := store.Events(l.Recorder.sessionID)
	var exts []TurnsExtended
	for _, e := range events {
		if e.Type == EvTurnsExtended {
			var x TurnsExtended
			_ = json.Unmarshal(e.Payload, &x)
			exts = append(exts, x)
		}
	}
	if len(exts) != 2 || exts[0].From != 2 || exts[0].To != 5 || exts[1].Count != 2 || exts[1].Max != 2 || exts[0].By != "user" {
		t.Fatalf("recorded: %+v", exts)
	}

	// A resumed conversation counts the continuations its record holds.
	l2, _, _ := harness(t, busy(20), policy.ModeDefault, true)
	l2.Config.MaxTurns, l2.Config.MaxContinues = 2, 2
	l2.NoteContinues(events)
	if _, err := l2.ContinueAfterLimit(ctx, "print"); !errors.Is(err, ErrContinueRefused) {
		t.Fatalf("resumed past the bound: %v", err)
	}

	// None allowed: refused, nothing recorded.
	l3, _, _ := harness(t, busy(20), policy.ModeDefault, true)
	l3.Config.MaxTurns = 1
	if reason, _ := l3.Run(ctx, "go"); reason != TermMaxTurns {
		t.Fatal(reason)
	}
	if _, err := l3.ContinueAfterLimit(ctx, "tui"); !errors.Is(err, ErrContinueRefused) {
		t.Fatalf("with none allowed: %v", err)
	}
}

// varied is n turns that each call a tool with a different argument, so the
// loop's repeat guard does not end the run before its limit.
func varied(n int) []scriptedTurn {
	out := make([]scriptedTurn, n)
	for i := range out {
		out[i] = scriptedTurn{calls: []model.ToolCall{call("glob", map[string]string{"pattern": fmt.Sprintf("*.none%d", i)})}}
	}
	return out
}
