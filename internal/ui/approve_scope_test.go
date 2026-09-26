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
	ok, err := a.Approve(ctx, "bash", nil, policy.Result{Scope: "bash(go test *)"})
	if err != nil || !ok || answer.By != agent.BySessionScope || answer.Scope != "bash(go test *)" {
		t.Fatalf("ok %v err %v answer %+v", ok, err, answer)
	}
}
