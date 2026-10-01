package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

type fakeMCP struct{ name, desc string }

func (f fakeMCP) Name() string            { return f.name }
func (f fakeMCP) Description() string     { return f.desc }
func (f fakeMCP) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f fakeMCP) Mutates() bool           { return true }
func (f fakeMCP) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	return tools.Result{Content: "ran"}
}

func TestDeferMCP(t *testing.T) {
	reg := tools.NewRegistry(tools.Read{})
	for i := range 5 {
		reg.Add(fakeMCP{fmt.Sprintf("mcp__s__t%d", i), fmt.Sprintf("does thing %d", i)})
	}
	if DeferMCP(reg, 5) {
		t.Fatal("deferred at the threshold")
	}
	reg.Add(fakeMCP{"mcp__s__weather", "reports the weather"})
	if !DeferMCP(reg, 5) {
		t.Fatal("not deferred past the threshold")
	}
	names := func() []string {
		var out []string
		for _, d := range reg.Definitions() {
			out = append(out, d.Name)
		}
		return out
	}
	if got := strings.Join(names(), ","); got != "read,tool_search" {
		t.Fatalf("offered %s", got)
	}
	// Hidden tools are still registered, so policy sees a call to one.
	if _, ok := reg.Get("mcp__s__t3"); !ok {
		t.Fatal("a deferred tool is not registered")
	}
	search, _ := reg.Get("tool_search")
	if search.Mutates() {
		t.Fatal("tool_search changes nothing")
	}
	res := search.Run(context.Background(), nil, json.RawMessage(`{"query":"weather"}`))
	if res.IsError || !strings.Contains(res.Content, "mcp__s__weather") || !strings.Contains(res.Content, "parameters") {
		t.Fatalf("%+v", res)
	}
	if got := strings.Join(names(), ","); !strings.Contains(got, "mcp__s__weather") || strings.Contains(got, "t3") {
		t.Fatalf("after the search: %s", got)
	}
	if r := search.Run(context.Background(), nil, json.RawMessage(`{}`)); !r.IsError {
		t.Fatal("an empty query was accepted")
	}
	if r := search.Run(context.Background(), nil, json.RawMessage(`{"query":"zzz"}`)); !strings.Contains(r.Content, "No tool matches") {
		t.Fatalf("%+v", r)
	}
}
