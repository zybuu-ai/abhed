package clitest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// MCPServer is a stub MCP server over HTTP with n tools, tool_000 onwards.
// Each call answers "called <tool>" and is counted.
type MCPServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []string
}

// NewMCPServer starts one on loopback.
func NewMCPServer(t testing.TB, n int) *MCPServer {
	t.Helper()
	m := &MCPServer{}
	tools := make([]map[string]any, n)
	for i := range tools {
		tools[i] = map[string]any{
			"name":        fmt.Sprintf("tool_%03d", i),
			"description": fmt.Sprintf("Stub tool number %d, which reports on widget %d in detail.", i, i),
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"widget": map[string]any{"type": "string", "description": "which widget to report on"}}},
		}
	}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var result any
		switch req.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "stub", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": tools}
		case "tools/call":
			m.mu.Lock()
			m.calls = append(m.calls, req.Params.Name)
			m.mu.Unlock()
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "called " + req.Params.Name}}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// URL is the server's endpoint.
func (m *MCPServer) URL() string { return m.srv.URL }

// Calls are the tools called, in order.
func (m *MCPServer) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// ConfigWithMCP is DefaultUserConfig with one MCP server named name at url.
func ConfigWithMCP(name, url string) string {
	return `{"sandbox":{"min_tier":"none"},"mcp":{"servers":[{"name":"` + name + `","url":"` + url + `","enabled":true}]},` +
		`"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"{{MODEL_URL}}","model":"stub-model","context_window":32768}}}}`
}
