package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// The approval card carries the supervisor's verdict and advice.
func TestApproveShowsTheSupervisorChip(t *testing.T) {
	var out strings.Builder
	a := NewApprover(&out)
	a.Prepare = func(context.Context) (func() (string, bool), func()) {
		return func() (string, bool) { return "2", true }, func() {}
	}
	req := agent.ActionRequested{CallID: "c1", Tool: "bash", Args: json.RawMessage(`{}`),
		Supervisor: &agent.SupervisorNote{Decision: "ask", Code: "unverified-host", Advice: "use a host the person named\x1b[2J"}}
	payload, _ := json.Marshal(req)
	ctx := agent.WithRequested(context.Background(), agent.Event{Type: agent.EvActionRequested, Payload: payload})
	res := policy.Result{Decision: policy.Ask, Step: "monitor", Reason: "supervisor: unverified-host — r"}
	if ok, _ := a.Approve(ctx, "bash", json.RawMessage(`{"command":"curl https://x.example"}`), res); ok {
		t.Fatal("approved on 2 (No)")
	}
	if !strings.Contains(out.String(), "supervisor: ask · unverified-host — use a host the person named") {
		t.Fatalf("no chip:\n%s", out.String())
	}
	if strings.Contains(out.String(), "\x1b[2J") {
		t.Fatal("the advice reached the terminal raw")
	}
}
