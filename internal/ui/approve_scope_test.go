package ui

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// A call a remembered "always allow" lets through says so to the loop, so the
// record does not credit a person who was not asked.
func TestRememberedScopeIsReportedToTheLoop(t *testing.T) {
	a := &Approver{In: strings.NewReader(""), Out: io.Discard, Style: NewStyle(io.Discard), Session: NewAllowList()}
	a.Session.Add("bash(go test *)")
	ctx, answer := agent.ExpectAnswer(context.Background())
	ok, err := a.Approve(ctx, "bash", nil, policy.Result{Decision: policy.Ask, Step: "default", Scope: "bash(go test *)"})
	if err != nil || !ok || answer.By != agent.BySessionScope || answer.Scope != "bash(go test *)" {
		t.Fatalf("ok %v err %v answer %+v", ok, err, answer)
	}
}

// An ask rule or a destructive command asks every time: no remembered scope
// satisfies it, and none is offered.
func TestAskRuleIgnoresARememberedScope(t *testing.T) {
	for _, step := range []string{"ask", "destructive", "screen"} {
		var out strings.Builder
		a := &Approver{In: strings.NewReader("r\n"), Out: &out, Style: NewStyle(io.Discard), Session: NewAllowList()}
		a.Session.Add("bash(git tag *)")
		ctx, answer := agent.ExpectAnswer(context.Background())
		res := policy.Result{Decision: policy.Ask, Step: step, Scope: "bash(git tag *)", Reason: "matched ask rule bash(git tag*)"}
		ok, err := a.Approve(ctx, "bash", nil, res)
		if err != nil || ok || answer.By == agent.BySessionScope {
			t.Fatalf("step %s: ok %v err %v answer %+v", step, ok, err, answer)
		}
		if strings.Contains(out.String(), "Always allow") {
			t.Fatalf("step %s offered always-allow: %q", step, out.String())
		}
	}
}

// Choosing [A]lways is recorded on the approval that granted it.
func TestAlwaysRecordsTheGrantedScope(t *testing.T) {
	a := &Approver{In: strings.NewReader("A\n"), Out: io.Discard, Style: NewStyle(io.Discard), Session: NewAllowList()}
	ctx, answer := agent.ExpectAnswer(context.Background())
	res := policy.Result{Decision: policy.Ask, Step: "default", Scope: "bash(mkdir *)"}
	if ok, err := a.Approve(ctx, "bash", nil, res); err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if answer.By != agent.ByReviewer || answer.Granted != "bash(mkdir *)" {
		t.Fatalf("answer %+v", answer)
	}
}
