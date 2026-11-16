package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// A denial by a rule tells the model whose rule it is, so it does not tell
// the person a capability is missing, and the record keeps the layer.
func TestDenialNamesItsSourceToTheModelAndTheRecord(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "curl -sI https://example.com"})}},
		{text: "done"},
	}, policy.ModeBypass, true)
	l.Policy.RuleSource = func(list, rule string) (string, string) {
		return "org", "the org policy (https://org.example, version 1)"
	}
	if err := l.Policy.AddDeny("bash(curl*)"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(context.Background(), "check the site"); err != nil {
		t.Fatal(err)
	}
	got := lastToolResult(l)
	for _, want := range []string{"denied by rule bash(curl*) from the org policy (https://org.example, version 1)", "not a missing capability"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the model was not told %q:\n%s", want, got)
		}
	}
	evs, _ := store.Events("sess1")
	found := false
	for _, ev := range evs {
		if ev.Type != EvActionDenied {
			continue
		}
		var d map[string]string
		_ = json.Unmarshal(ev.Payload, &d)
		found = d["source"] == "org" && strings.Contains(d["reason"], "from the org policy")
	}
	if !found {
		t.Fatal("the denial's record does not name its source")
	}
}
