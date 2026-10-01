package config

import (
	"slices"
	"strings"
	"testing"
)

// Suggestions are on by default; an untrusted workspace may turn them off
// but may not choose the model they are asked of.
func TestSuggestWorkspaceOnlyTurnsItOff(t *testing.T) {
	if !Default().Suggest.Enabled {
		t.Fatal("suggest.enabled is off by default")
	}
	_, ws := trustHome(t, `{"model":{"default":"local","providers":{"small":{"type":"openai-compatible","base_url":"http://localhost:1/v1","model":"s","context_window":8192}}}}`,
		`{"suggest":{"enabled":false,"model":"small"}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Suggest.Enabled || !slices.Contains(cfg.Workspace.Applied, "suggest.enabled") {
		t.Fatalf("the workspace could not turn suggestions off: %+v", cfg.Workspace)
	}
	if cfg.Suggest.Model != "" || !ignored(cfg.Workspace, "suggest.model") {
		t.Fatalf("an untrusted workspace chose the suggestion model: %q", cfg.Suggest.Model)
	}
}

// suggest.model names a configured provider or the configuration is refused.
func TestSuggestModelMustBeConfigured(t *testing.T) {
	c := Default()
	c.Suggest.Model = "nope"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "suggest.model") {
		t.Fatalf("Validate = %v", err)
	}
	c.Suggest.Model = "local"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
