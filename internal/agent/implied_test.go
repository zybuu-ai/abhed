package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// What a call runs as once the repository's configuration is applied is
// judged too: an org deny on a force push refuses a plain git push the
// configuration makes one; with no rule the destructive confirmation asks;
// nothing it says ever allows more.
func TestImpliedLineTightensOnly(t *testing.T) {
	hostAsk := policy.Result{Decision: policy.Ask, Step: "hostgit_each", Reason: "git push runs through host git as you"}
	deny := policy.New(policy.ModeDefault)
	if err := deny.AddDeny("bash(git push -f*)"); err != nil {
		t.Fatal(err)
	}
	implied := "git push --force --mirror origin"
	if got := impliedJudged(hostAsk, implied, deny.Evaluate("bash", true, impliedArgs(implied))); got.Decision != policy.Deny ||
		!strings.Contains(got.Reason, "git configuration makes this git push --force") {
		t.Fatalf("org deny: %+v", got)
	}
	plain := policy.New(policy.ModeDefault)
	got := impliedJudged(hostAsk, implied, plain.Evaluate("bash", true, impliedArgs(implied)))
	if got.Decision != policy.Ask || got.Step != "destructive" || got.Scope != "" || !strings.Contains(got.Reason, "host git as you") {
		t.Fatalf("no rule: %+v", got)
	}
	// In bypass mode the implied line is allowed, and the host ask stands.
	by := policy.New(policy.ModeBypass)
	if got := impliedJudged(hostAsk, "git push --delete origin", by.Evaluate("bash", true, impliedArgs("git push --delete origin"))); got.Decision != policy.Ask && got.Decision != policy.Deny {
		t.Fatalf("bypass widened it: %+v", got)
	}
	// An allowed implied line never widens a deny or an ask.
	allow := policy.New(policy.ModeDefault)
	if err := allow.AddAllow("bash(git push *)"); err != nil {
		t.Fatal(err)
	}
	if got := impliedJudged(hostAsk, "git push origin main", allow.Evaluate("bash", true, impliedArgs("git push origin main"))); got != hostAsk {
		t.Fatalf("an allow changed the decision: %+v", got)
	}
}

// impliedBash is a bash whose every line the repository makes a mirror push.
type impliedBash struct{ ran *bool }

func (impliedBash) Name() string            { return "bash" }
func (impliedBash) Description() string     { return "bash" }
func (impliedBash) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (impliedBash) Mutates() bool           { return true }
func (b impliedBash) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	*b.ran = true
	return tools.Result{Content: "pushed"}
}
func (impliedBash) ImpliedHost(*tools.Session, json.RawMessage) string {
	return "git push --force --mirror origin"
}

// The loop judges the implied line: an org deny on force pushes refuses a
// plain git push the repository's configuration makes a mirror push.
func TestLoopRefusesAConfigImpliedForcePush(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{{calls: []model.ToolCall{call("bash", map[string]string{"command": "git push"})}}, {text: "done"}},
		policy.ModeBypass, true)
	ran := false
	l.Tools = tools.NewRegistry(impliedBash{ran: &ran})
	if err := l.Policy.AddDeny("bash(git push -f*)"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(context.Background(), "push it"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("sess1")
	if ran || !hasEvent(evs, EvActionDenied) {
		for _, e := range evs {
			t.Logf("%s %.200s", e.Type, e.Payload)
		}
		t.Fatalf("a plain git push the configuration forces ran (%v) or was not denied", ran)
	}
}
