package ui

import (
	"encoding/json"
	"slices"
	"testing"
)

// A statusline command reads the model as JSON, so its keys are an interface:
// renaming one breaks every script that reads it. The cost is absent unless
// the provider is priced, rather than a misleading zero.
func TestStatusModelJSONKeys(t *testing.T) {
	data, err := json.Marshal(StatusModel{Record: RecordMemory})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"background_tasks", "context_percent", "context_tokens", "mode", "mode_locked", "model",
		"network", "provider", "record", "sandbox_tier", "tokens_in", "tokens_out", "waiting_ask"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys = %v\nwant %v", keys, want)
	}
	price := 0.25
	data, _ = json.Marshal(StatusModel{CostUSD: &price, SessionName: "fix", GitBranch: "main"})
	_ = json.Unmarshal(data, &got)
	if got["cost_usd"] != 0.25 || got["session_name"] != "fix" || got["git_branch"] != "main" {
		t.Fatalf("optional keys: %v", got)
	}
}
