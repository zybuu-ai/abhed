package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// plainTool is a tool that is not an MCP server's, under any name.
type plainTool struct{ name string }

func (p plainTool) Name() string          { return p.name }
func (plainTool) Description() string     { return "" }
func (plainTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (plainTool) Mutates() bool           { return true }
func (plainTool) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	return tools.Result{}
}

// A tool refused at run time is refused for its own reason, a wire-name clash
// included, and the answer says which.
func TestRuntimeMCPAddSaysWhyAToolIsRefused(t *testing.T) {
	gw := mcp.NewGateway()
	t.Cleanup(gw.Close)
	g := newHookRig(t, nil, func(o *Options) {
		o.Gateway = gw
		o.Registry = tools.NewRegistry(tools.Read{}, plainTool{model.WireName("mcp__new__a.b")})
	})
	alice := g.signIn(t, "alice")
	body, _ := json.Marshal(map[string]string{"name": "new", "url": fakeMCP(t, "a.b", "c")})
	rec := g.do(alice, "POST", "/v1/admin/mcp", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		Refused  []string `json:"refused"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Refused) != 1 || len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "would reach the model provider as") {
		t.Fatalf("refused %v, warnings %q", resp.Refused, resp.Warnings)
	}
}

// An add refused for an overlapping name takes the tools it leaves out of
// tool_search too, not only out of the registry.
func TestRefusedAddLeavesNoStaleSearch(t *testing.T) {
	gw := mcp.NewGateway()
	t.Cleanup(gw.Close)
	g := newHookRig(t, nil, func(o *Options) { o.Gateway = gw })
	alice := g.signIn(t, "alice")
	names := []string{"b__x"}
	for i := range 44 {
		names = append(names, fmt.Sprintf("t%d", i))
	}
	body, _ := json.Marshal(map[string]string{"name": "a", "url": fakeMCP(t, names...)})
	if rec := g.do(alice, "POST", "/v1/admin/mcp", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	search, ok := g.srv.state.toolRegistry().Get("tool_search")
	query := json.RawMessage(`{"query":"mcp__a__b__x","limit":1}`)
	if !ok || strings.HasPrefix(search.Run(context.Background(), nil, query).Content, "No tool matches") {
		t.Fatal("setup: a's 45 tools should be behind tool_search")
	}
	// a__b's tool x would be named mcp__a__b__x: refused, and a's b__x left out.
	body, _ = json.Marshal(map[string]string{"name": "a__b", "url": fakeMCP(t, "x")})
	if rec := g.do(alice, "POST", "/v1/admin/mcp", string(body)); rec.Code != http.StatusBadRequest {
		t.Fatalf("overlapping add: %d %s", rec.Code, rec.Body)
	}
	reg := g.srv.state.toolRegistry()
	if _, ok := reg.Get("mcp__a__b__x"); ok {
		t.Fatal("setup: the overlapping name should leave the registry")
	}
	search, _ = reg.Get("tool_search")
	if res := search.Run(context.Background(), nil, query); !strings.HasPrefix(res.Content, "No tool matches") {
		t.Fatalf("tool_search still offers a tool taken out: %s", res.Content)
	}
}
