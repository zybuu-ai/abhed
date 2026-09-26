package hawkeye

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// markOf returns the mark the text report gives the call at seq.
func markOf(t *testing.T, r Report, seq int64) string {
	t.Helper()
	for _, line := range strings.Split(Text(r), "\n") {
		f := strings.Fields(line)
		if len(f) > 2 && f[1] == "#"+strconv.FormatInt(seq, 10) {
			return f[0]
		}
	}
	t.Fatalf("no call line for #%d:\n%s", seq, Text(r))
	return ""
}

// A call with no result on the record did not run, whatever it was allowed.
func TestACallWithNoResultIsNotShownAsRun(t *testing.T) {
	r := (&rec{}).user("go").model(1200, 900, 32768)
	r.add(agent.EvActionRequested, agent.ActorAgent, agent.Trusted, agent.ActionRequested{CallID: "c1", Tool: "write", Args: json.RawMessage(`{"path":"x"}`), RequiresApproval: true})
	r.add(agent.EvActionApproved, agent.ActorSystem, agent.Trusted, map[string]string{"call_id": "c1", "step": "default", "by": "reviewer"})
	// An interrupted request, as the loop now records one, and one from before it did.
	r.add(agent.EvActionRequested, agent.ActorAgent, agent.Trusted, agent.ActionRequested{CallID: "c2", Tool: "write", Args: json.RawMessage(`{"path":"y"}`), RequiresApproval: true})
	got := Analyze("s", r.end(agent.TermShutdown).evs)
	if got.Calls[0].Ran || markOf(t, got, got.Calls[0].Seq) != "-" {
		t.Errorf("an allowed call with no result is shown as run: %+v", got.Calls[0])
	}
	if markOf(t, got, got.Calls[1].Seq) != "-" {
		t.Errorf("a pending call is shown as run")
	}
	if html, err := HTML(got); err != nil || !strings.Contains(html, "not run") {
		t.Errorf("the page does not say the call did not run: %v", err)
	}
}

// A command the sandbox refused part of is marked, and raises a finding.
func TestASandboxRefusalIsAFinding(t *testing.T) {
	out := "exit 0 · 58ms\nls: .abhed: Operation not permitted\n\n\nNOTE: the sandbox denied this operation. Retrying the same command will fail identically."
	r := (&rec{}).user("look").model(1200, 900, 32768)
	for _, c := range []struct{ id, out, tier string }{
		{"c1", out, "process"}, {"c2", "exit 0 · 3ms\na", "process"}, {"c3", out, "none"}, {"c4", out, ""},
	} {
		r.add(agent.EvActionRequested, agent.ActorAgent, agent.Trusted, agent.ActionRequested{CallID: c.id, Tool: "bash", Args: json.RawMessage(`{"command":"ls .abhed"}`)})
		r.add(agent.EvActionApproved, agent.ActorSystem, agent.Trusted, map[string]string{"call_id": c.id, "step": "mode", "by": "policy"})
		r.add(agent.EvObservation, agent.ActorTool, agent.Untrusted, agent.Observation{CallID: c.id, Tool: "bash", Content: c.out, Sandbox: c.tier})
	}
	got := Analyze("s", r.end(agent.TermCompleted).evs)
	// Only the refusal under a sandbox counts: on the host, or with no tier on the record, it is not one.
	for i, want := range []bool{true, false, false, false} {
		if got.Calls[i].SandboxDenied != want {
			t.Errorf("call %s sandbox refused %v, want %v", got.Calls[i].CallID, got.Calls[i].SandboxDenied, want)
		}
	}
	if m := markOf(t, got, got.Calls[0].Seq); m != "!" {
		t.Errorf("a sandbox refusal is marked %q", m)
	}
	if m := markOf(t, got, got.Calls[1].Seq); m != "✓" {
		t.Errorf("a clean run is marked %q", m)
	}
	if f := has(got, "sandbox-denied"); f == nil || f.Seq != got.Calls[0].Seq {
		t.Errorf("no sandbox-denied finding: %+v", got.Findings)
	}
}

// Who decided is what the event says, not a guess from its actor.
func TestAttributionIsTheEventsOwn(t *testing.T) {
	r := (&rec{}).user("go").model(1200, 900, 32768)
	deny := func(id string, actor agent.Actor, p map[string]string) {
		r.add(agent.EvActionRequested, actor, agent.Trusted, agent.ActionRequested{CallID: id, Tool: "bash", Args: json.RawMessage(`{"command":"rm x"}`)})
		p["call_id"] = id
		r.add(agent.EvActionDenied, actor, agent.Trusted, p)
	}
	// The person's own [y/N] decline at the line terminal.
	deny("u1", agent.ActorUser, map[string]string{"step": "destructive", "by": "user", "reason": "rejected: destructive"})
	// A run with no one to ask.
	r.add(agent.EvActionRequested, agent.ActorAgent, agent.Trusted, agent.ActionRequested{CallID: "c2", Tool: "write", Args: json.RawMessage(`{}`)})
	r.add(agent.EvActionDenied, agent.ActorSystem, agent.Trusted, map[string]string{"call_id": "c2", "step": "default", "by": "headless"})
	// A remembered scope.
	r.add(agent.EvActionRequested, agent.ActorAgent, agent.Trusted, agent.ActionRequested{CallID: "c3", Tool: "write", Args: json.RawMessage(`{}`)})
	r.add(agent.EvActionApproved, agent.ActorSystem, agent.Trusted, map[string]string{"call_id": "c3", "step": "default", "by": "session-scope", "scope": "write(*)"})
	r.add(agent.EvObservation, agent.ActorTool, agent.Untrusted, agent.Observation{CallID: "c3", Tool: "write", Content: "ok"})
	// A reviewer's refusal written before denials carried "by".
	r.add(agent.EvActionRequested, agent.ActorAgent, agent.Trusted, agent.ActionRequested{CallID: "c4", Tool: "write", Args: json.RawMessage(`{}`)})
	r.add(agent.EvActionDenied, agent.ActorUser, agent.Trusted, map[string]string{"call_id": "c4", "step": "default"})
	got := Analyze("s", r.end(agent.TermCompleted).evs)

	want := []struct{ by, scope string }{{"user", ""}, {"headless", ""}, {"session-scope", "write(*)"}, {"reviewer", ""}}
	for i, w := range want {
		if c := got.Calls[i]; c.By != w.by || c.Scope != w.scope {
			t.Errorf("call %s by %q scope %q, want %q %q", c.CallID, c.By, c.Scope, w.by, w.scope)
		}
	}
	if got.Policy.Reviewer != 1 {
		t.Errorf("%d asked a reviewer, want only the one that did", got.Policy.Reviewer)
	}
	for _, f := range got.Findings {
		if f.Code == "denied" && f.Seq == got.Calls[0].Seq && !strings.Contains(f.Detail, "by user") {
			t.Errorf("the person's own decline reads %q", f.Detail)
		}
	}
}

// Where the record names who settled a call, the report does too, with the
// scope they chose to always allow.
func TestTheReportNamesTheApprover(t *testing.T) {
	r := (&rec{}).user("go").model(1200, 900, 32768)
	r.add(agent.EvActionRequested, agent.ActorAgent, agent.Trusted, agent.ActionRequested{CallID: "c1", Tool: "bash", Args: json.RawMessage(`{"command":"mkdir a"}`)})
	r.add(agent.EvActionApproved, agent.ActorUser, agent.Trusted, map[string]string{"call_id": "c1", "step": "default", "by": "reviewer", "approver": "olga@example.com", "granted_scope": "bash(mkdir *)"})
	r.add(agent.EvActionRequested, agent.ActorAgent, agent.Trusted, agent.ActionRequested{CallID: "c2", Tool: "bash", Args: json.RawMessage(`{"command":"git tag -d v1"}`)})
	r.add(agent.EvActionDenied, agent.ActorUser, agent.Trusted, map[string]string{"call_id": "c2", "step": "destructive", "by": "reviewer", "approver": "olga@example.com", "reason": "rejected: delete or replace a tag"})
	got := Analyze("s", r.end(agent.TermCompleted).evs)
	if c := got.Calls[0]; c.Approver != "olga@example.com" || c.GrantedScope != "bash(mkdir *)" {
		t.Errorf("call %+v", c)
	}
	if f := has(got, "denied"); f == nil || !strings.Contains(f.Detail, "by reviewer olga@example.com") {
		t.Errorf("the refusal does not name who refused: %+v", f)
	}
	if html, err := HTML(got); err != nil || !strings.Contains(html, "olga@example.com") || !strings.Contains(html, "always allowing bash(mkdir *)") {
		t.Errorf("the page does not name the approver or the scope: %v", err)
	}
}
