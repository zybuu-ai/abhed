package hawkeye

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A policy layer an edition added is reported as a layer of its own, and
// the decisions its rules made are counted under it and named on each call.
func TestPolicyLayerIsReported(t *testing.T) {
	r := (&rec{}).add(agent.EvPolicyLayer, agent.ActorSystem, agent.Trusted, map[string]any{
		"layer": "workspace pay", "revision": "4", "sha256": "abc", "deny": []string{"bash(curl*)"},
		"ask": []string{}, "allow": []string{}, "set_aside": []string{"bash(make*)"},
	}).user("x")
	r.add(agent.EvActionRequested, agent.ActorAgent, agent.Trusted, agent.ActionRequested{
		CallID: "c1", Tool: "bash", Args: []byte(`{"command":"curl x"}`)})
	r.add(agent.EvActionDenied, agent.ActorSystem, agent.Trusted, map[string]string{
		"call_id": "c1", "step": "deny", "by": "policy", "rule": "bash(curl*)", "layer": "workspace pay",
		"reason": "denied by workspace pay rule bash(curl*)"})
	got := Analyze("s-layer", r.end(agent.TermCompleted).evs)
	if len(got.PolicyLayers) != 1 || got.PolicyLayers[0].Name != "workspace pay" || got.PolicyLayers[0].Revision != "4" ||
		len(got.PolicyLayers[0].SetAside) != 1 {
		t.Fatalf("layers = %+v", got.PolicyLayers)
	}
	if got.Policy.ByLayer["workspace pay"] != 1 || len(got.Calls) != 1 || got.Calls[0].Layer != "workspace pay" ||
		got.Calls[0].Rule != "bash(curl*)" {
		t.Fatalf("policy = %+v calls = %+v", got.Policy, got.Calls)
	}
	text := Text(got)
	if !strings.Contains(text, "layer     workspace pay (revision 4)") || !strings.Contains(text, "decided 1 call(s)") ||
		!strings.Contains(text, "1 allow rule(s) set aside") {
		t.Fatalf("text report:\n%s", text)
	}
	page, err := HTML(got)
	if err != nil || !strings.Contains(page, "Policy layers") || !strings.Contains(page, "rule of workspace pay") {
		t.Fatalf("html report (%v) lacks the layer", err)
	}
}
