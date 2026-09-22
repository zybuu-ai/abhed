package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The agent's tools cannot reach Abhed's own state, whatever the mode: the
// policy that governs the agent must not be writable by the agent.
func TestToolsCannotTouchHarnessState(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, StateDir)
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(state, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"permissions":{"deny":["bash(*shutdown*)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	users := filepath.Join(state, "users.json")

	calls := []struct {
		tool Tool
		args map[string]any
	}{
		{Read{}, map[string]any{"path": cfg}},
		{Write{}, map[string]any{"path": cfg, "content": `{"permissions":{"deny":[]}}`}},
		{Write{}, map[string]any{"path": users, "content": `[{"user":"intruder"}]`}},
		{Edit{}, map[string]any{"path": cfg, "old_string": "shutdown", "new_string": "nothing"}},
		{Glob{}, map[string]any{"pattern": "*.json", "path": state}},
		{Grep{}, map[string]any{"pattern": "deny", "path": state}},
		// A relative spelling and a detour through a sibling both name the same place.
		{Read{}, map[string]any{"path": filepath.Join(dir, "src", "..", StateDir, "config.json")}},
	}
	for _, c := range calls {
		raw, _ := json.Marshal(c.args)
		res := c.tool.Run(context.Background(), s, raw)
		if !res.IsError || !strings.Contains(res.Content, "own state") {
			t.Errorf("%s %s: not refused: %q", c.tool.Name(), c.args["path"], res.Content)
		}
	}
	if got, _ := os.ReadFile(cfg); !strings.Contains(string(got), "shutdown") {
		t.Fatalf("the configuration was changed: %s", got)
	}
	if _, err := os.Stat(users); err == nil {
		t.Fatal("a users file was planted")
	}
}
