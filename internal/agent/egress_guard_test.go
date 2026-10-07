package agent

import (
	"context"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// The loop's model requests and tool calls name its tool set's guard, and
// a subagent's loop, which copies the config, names the same one.
func TestLoopNamesItsSetsGuard(t *testing.T) {
	g := egress.NewGuard(egress.GuardOptions{Policy: nil})
	defer g.Close()
	l := &Loop{Config: Config{Egress: g}}
	if got := egress.CallerOf(l.withCaller(context.Background())).Guard; got != g {
		t.Fatalf("model requests name %p, want %p", got, g)
	}
	if got := egress.CallerOf(l.withLaunch(context.Background(), "call-1")); got.Guard != g || got.CallID != "call-1" {
		t.Fatalf("tool calls name %+v", got)
	}
	open := &Loop{Config: Config{Egress: egress.Unguarded}}
	if got := egress.CallerOf(open.withCaller(context.Background())).Guard; got != egress.Unguarded {
		t.Fatal("a loop outside the allowlist does not say so")
	}
}
