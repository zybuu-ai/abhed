package policy

import (
	"encoding/json"
	"testing"
)

// AllowRuleFor is the rule the prompt would offer, and none for a call no
// allow rule would approve.
func TestAllowRuleFor(t *testing.T) {
	e := New(ModePlan)
	if err := e.AddAsk("bash(make deploy*)"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		tool, args, want string
	}{
		{"bash", `{"command":"go test ./..."}`, "bash(go test ./...)"},
		{"bash", `{"command":"make deploy"}`, ""},       // an ask rule holds it
		{"bash", `{"command":"rm -rf build && x"}`, ""}, // a destructive command always asks
		{"mcp__tickets__get_ticket", `{"id":"T-1"}`, "mcp__tickets__get_ticket"},
	} {
		if got := e.AllowRuleFor(c.tool, true, json.RawMessage(c.args)); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.tool, c.args, got, c.want)
		}
	}
}
