package hawkeye

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A model may reuse a call id in a later turn (an OpenAI-compatible server
// numbering each turn's calls from call_0_0). Each request is a call of its
// own: a denial in one turn is not taken over by an approval in the next,
// and the review before the second goes to the second.
func TestACallIDReusedAcrossTurnsKeepsEachCall(t *testing.T) {
	r := &rec{}
	r.user("first")
	r.model(100, 0, 131072)
	r.denied("call_0_0", "bash", `{"command":"echo refused"}`, "managed", "denied by the managed policy")
	r.user("second")
	r.model(100, 0, 131072)
	r.add(agent.EvMonitorVerdict, agent.ActorSystem, agent.Trusted, agent.MonitorVerdict{
		CallID: "call_0_0", Before: "allow", After: "ask", Code: "scope", Rationale: "touches the workspace"})
	r.call("call_0_0", "bash", `{"command":"touch second-turn.txt"}`, "default", "", false)
	r.end(agent.TermCompleted)

	rep := Analyze("s-test", r.evs)
	if len(rep.Calls) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(rep.Calls), rep.Calls)
	}
	first, second := rep.Calls[0], rep.Calls[1]
	if first.Decision != "denied" || first.Ran || !strings.Contains(first.Subject, "echo refused") || first.Step != "managed" {
		t.Errorf("the first turn's call: %+v", first)
	}
	if second.Decision != "allowed" || !second.Ran || !strings.Contains(second.Subject, "second-turn") {
		t.Errorf("the second turn's call: %+v", second)
	}
	if rep.Policy.Denied != 1 || rep.Policy.Allowed != 1 {
		t.Errorf("policy: %d allowed, %d denied; want 1 and 1", rep.Policy.Allowed, rep.Policy.Denied)
	}
	if rep.Supervisor == nil || len(rep.Supervisor.Concerns) != 1 || !strings.Contains(rep.Supervisor.Concerns[0].Subject, "second-turn") {
		t.Errorf("the review went to the wrong call: %+v", rep.Supervisor)
	}
	text := Text(rep)
	if !strings.Contains(text, "1 allowed · 1 denied") {
		t.Errorf("the summary hides the denial:\n%s", text)
	}
	if strings.Count(text, "second-turn") > 2 || !strings.Contains(text, "echo refused") {
		t.Errorf("the calls are not listed once each:\n%s", text)
	}
	b, _ := json.Marshal(rep)
	if !strings.Contains(string(b), `"decision":"denied"`) {
		t.Error("the API's report lost the denial")
	}
}
