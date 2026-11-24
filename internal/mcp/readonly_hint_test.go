package mcp

import (
	"encoding/json"
	"testing"
)

// #19: readOnlyHint is believed only for a server the operator configured.
// A tool it marks read-only is then not a mutating call; every other tool,
// and every tool of a server a person or a workspace added, still is.
func TestReadOnlyHintOnlyFromTheOperator(t *testing.T) {
	var defs []ToolDef
	if err := json.Unmarshal([]byte(`[
		{"name":"search","annotations":{"readOnlyHint":true}},
		{"name":"write_page","annotations":{"readOnlyHint":false}},
		{"name":"plain"}]`), &defs); err != nil {
		t.Fatal(err)
	}
	mutates := func(hints bool) map[string]bool {
		g := NewGateway()
		g.clients["docs"] = &Client{name: "docs", tools: defs}
		g.configs["docs"] = ServerConfig{Name: "docs", ReadOnlyHints: hints}
		out := map[string]bool{}
		for _, tl := range g.Tools() {
			out[tl.Name()] = tl.Mutates()
		}
		return out
	}
	op := mutates(true)
	if op["mcp__docs__search"] || !op["mcp__docs__write_page"] || !op["mcp__docs__plain"] {
		t.Fatalf("operator's server: %v", op)
	}
	for name, m := range mutates(false) {
		if !m {
			t.Errorf("a person's server's hint was believed for %s", name)
		}
	}
}
