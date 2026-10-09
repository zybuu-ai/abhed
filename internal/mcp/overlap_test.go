package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Server a's tool b__x and server a__b's tool x are both mcp__a__b__x, so a
// rule written for one would reach the other: neither is offered.
func TestToolsWhoseNamesAnotherServerClaimsAreLeftOut(t *testing.T) {
	g := NewGateway()
	g.clients["a"] = &Client{name: "a", tools: []ToolDef{{Name: "b__x"}, {Name: "y"}}}
	g.clients["a__b"] = &Client{name: "a__b", tools: []ToolDef{{Name: "x"}}}
	g.clients["ab"] = &Client{name: "ab", tools: []ToolDef{{Name: "z"}}}
	var names []string
	for _, tl := range g.Tools() {
		names = append(names, tl.Name())
	}
	if strings.Join(names, ",") != "mcp__a__y,mcp__ab__z" {
		t.Fatalf("offered %v", names)
	}
	o := g.Overlaps()
	if len(o) != 1 || !strings.Contains(o[0], `server "a"`) || !strings.Contains(o[0], `server "a__b"`) {
		t.Fatalf("overlaps %q", o)
	}
}

// A server whose name is another's and "__" is not started.
func TestOverlappingServerIsNotStarted(t *testing.T) {
	g := NewGateway()
	errs := g.Connect(context.Background(), []ServerConfig{
		{Name: "a", Command: "/nonexistent/abhed-test", Enabled: true},
		{Name: "a__b", Command: "/nonexistent/abhed-test", Enabled: true},
		{Name: "a_b", Command: "/nonexistent/abhed-test", Enabled: true},
	})
	var overlap int
	for _, err := range errs {
		if strings.Contains(err.Error(), "is not started: its name begins with that of server") {
			overlap++
			if !strings.Contains(err.Error(), `"a__b"`) || !strings.Contains(err.Error(), `"a"`) {
				t.Errorf("error %v", err)
			}
		}
	}
	if overlap != 1 {
		t.Fatalf("errors %v", errs)
	}
	if OverlappedBy("a", []string{"a__b"}) != "" || OverlappedBy("a__b", []string{"a"}) != "a" {
		t.Error("OverlappedBy")
	}
}

// fakeHTTPServer is an MCP server over HTTP offering the named tools.
func fakeHTTPServer(t *testing.T, tools ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := `{"protocolVersion":"2024-11-05","serverInfo":{"name":"t","version":"1"},"capabilities":{"tools":{}}}`
		if req.Method == "tools/list" {
			var defs []string
			for _, n := range tools {
				defs = append(defs, `{"name":"`+n+`","inputSchema":{"type":"object"}}`)
			}
			result = `{"tools":[` + strings.Join(defs, ",") + `]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp"
}

func toolNames(g *Gateway) string {
	var names []string
	for _, tl := range g.Tools() {
		names = append(names, tl.Name())
	}
	return strings.Join(names, ",")
}

// A server refused for its name still claims it: the other server's tool
// named as one of its would be is left out, so a rule written for the
// refused server cannot approve it.
func TestRefusedServerStillClaimsItsNames(t *testing.T) {
	g := NewGateway()
	defer g.Close()
	errs := g.Connect(context.Background(), []ServerConfig{
		{Name: "a", URL: fakeHTTPServer(t, "b__x", "y"), Enabled: true},
		{Name: "a__b", URL: fakeHTTPServer(t, "x"), Enabled: true},
	})
	if len(errs) != 1 {
		t.Fatalf("errors %v", errs)
	}
	if got := toolNames(g); got != "mcp__a__y" {
		t.Fatalf("offered %s", got)
	}
}
