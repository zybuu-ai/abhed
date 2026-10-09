package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/mcp"
)

// fakeMCP is an MCP server over HTTP offering the named tools.
func fakeMCP(t *testing.T, names ...string) string {
	t.Helper()
	return slowMCP(t, 0, names...)
}

// slowMCP is fakeMCP taking delay to answer initialize.
func slowMCP(t *testing.T, delay time.Duration, names ...string) string {
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
		if req.Method == "initialize" {
			time.Sleep(delay)
		}
		if req.Method == "tools/list" {
			var defs []string
			for _, n := range names {
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

// A server added at run time whose name overlaps a running one's is refused
// with the reason, either way round, and no tool under the overlapping name
// stays registered.
func TestRuntimeMCPAddRefusesOverlappingNames(t *testing.T) {
	for _, c := range []struct {
		first, second string
		firstTools    []string
		secondTools   []string
		want          []string
	}{
		// The longer name added second: a's b__x is how a__b's x is named.
		{"a", "a__b", []string{"b__x", "y"}, []string{"x"}, []string{"mcp__a__y"}},
		// The shorter name added second: a__b keeps its tools.
		{"a__b", "a", []string{"x"}, []string{"b__x", "y"}, []string{"mcp__a__b__x"}},
	} {
		gw := mcp.NewGateway()
		t.Cleanup(gw.Close)
		g := newHookRig(t, nil, func(o *Options) { o.Gateway = gw })
		alice := g.signIn(t, "alice")
		add := func(name string, tools []string) *httptest.ResponseRecorder {
			body, _ := json.Marshal(map[string]string{"name": name, "url": fakeMCP(t, tools...)})
			return g.do(alice, "POST", "/v1/admin/mcp", string(body))
		}
		if rec := add(c.first, c.firstTools); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.first, rec.Code, rec.Body)
		}
		rec := add(c.second, c.secondTools)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "overlap") {
			t.Fatalf("%s after %s: %d %s", c.second, c.first, rec.Code, rec.Body)
		}
		var mcpTools []string
		for _, n := range g.srv.state.toolRegistry().Names() {
			if strings.HasPrefix(n, "mcp__") {
				mcpTools = append(mcpTools, n)
			}
		}
		slices.Sort(mcpTools)
		if !slices.Equal(mcpTools, c.want) {
			t.Errorf("%s then %s: registered %v, want %v", c.first, c.second, mcpTools, c.want)
		}
	}
}

// Two servers whose names overlap, added at the same moment while each is
// slow to start, cannot both start: exactly one is refused, and no tool is
// registered under the other server's name.
func TestConcurrentMCPAddsOfOverlappingNames(t *testing.T) {
	gw := mcp.NewGateway()
	t.Cleanup(gw.Close)
	g := newHookRig(t, nil, func(o *Options) { o.Gateway = gw })
	alice := g.signIn(t, "alice")
	urls := map[string]string{
		"a":    slowMCP(t, 300*time.Millisecond, "b__x", "y"),
		"a__b": slowMCP(t, 300*time.Millisecond, "x"),
	}
	codes := map[string]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, u := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, _ := json.Marshal(map[string]string{"name": name, "url": u})
			rec := g.do(alice, "POST", "/v1/admin/mcp", string(body))
			mu.Lock()
			codes[name] = rec.Code
			mu.Unlock()
		}()
	}
	wg.Wait()
	if (codes["a"] == http.StatusOK) == (codes["a__b"] == http.StatusOK) {
		t.Fatalf("codes %v: exactly one should start", codes)
	}
	want := map[string]string{"mcp__a__y": "a\x00y"}
	if codes["a__b"] == http.StatusOK {
		want = map[string]string{"mcp__a__b__x": "a__b\x00x"}
	}
	got := map[string]string{}
	for _, tl := range g.srv.state.toolRegistry().All() {
		if id := mcpIdentity(tl); id != "" {
			got[tl.Name()] = id
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("codes %v: registered %q, want %q", codes, got, want)
	}
}
