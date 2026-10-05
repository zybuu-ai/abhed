package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// mcpStub is a Streamable HTTP MCP server that can be taken down and back up.
// It offers one plain tool and one whose name is refused for its characters.
func mcpStub(t *testing.T) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	down := &atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := `{}`
		switch req.Method {
		case "initialize":
			result = `{"protocolVersion":"2024-11-05","serverInfo":{"name":"stub","version":"1"},"capabilities":{"tools":{}}}`
		case "tools/list":
			result = `{"tools":[{"name":"lookup","inputSchema":{"type":"object"}},{"name":"evil\u202eexe","inputSchema":{"type":"object"}}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	}))
	t.Cleanup(srv.Close)
	return srv, down
}

func mcpEntry(t *testing.T, caps map[string]any, name string) map[string]any {
	t.Helper()
	for _, x := range caps["mcp"].([]any) {
		if m := x.(map[string]any); m["name"] == name {
			return m
		}
	}
	t.Fatalf("no MCP server %s in %v", name, caps["mcp"])
	return nil
}

// §6.4 mcp/restart: the person reconnects one configured server; it is
// recorded mcp.status by: user, refused for an unknown name, and the
// capabilities say why a tool is missing.
func TestStudioMCPRestart(t *testing.T) {
	srv, down := mcpStub(t)
	r := newStudioRig(t, `,"mcp":{"servers":[{"name":"stub","url":"`+srv.URL+`","enabled":true}]}`)
	if !slices.Contains(r.cl.conn.acpFeatures(), "mcp.restart") {
		t.Fatalf("features: %v", r.cl.conn.acpFeatures())
	}
	id := r.open()
	var caps map[string]any
	r.cl.ok("_abhed/capabilities", map[string]any{"sessionId": id}, &caps)
	m := mcpEntry(t, caps, "stub")
	refused, _ := m["refusedTools"].([]any)
	if m["status"] != "connected" || len(refused) != 1 || refused[0] != "evil⟨U+202E⟩exe" {
		t.Fatalf("mcp before: %v", m)
	}

	var res map[string]any
	r.cl.ok("_abhed/mcp/restart", map[string]any{"sessionId": id, "name": "stub"}, &res)
	if res["status"] != "connected" || res["error"] != nil {
		t.Fatalf("restart: %v", res)
	}
	down.Store(true)
	r.cl.ok("_abhed/mcp/restart", map[string]any{"sessionId": id, "name": "stub"}, &res)
	if res["status"] != "error" || res["error"] == nil || res["error"] == "" {
		t.Fatalf("restart of a server that is down: %v", res)
	}
	r.cl.ok("_abhed/capabilities", map[string]any{"sessionId": id}, &caps)
	if m := mcpEntry(t, caps, "stub"); m["status"] != "error" || m["error"] == nil {
		t.Fatalf("mcp after a failed restart: %v", m)
	}
	down.Store(false)
	r.cl.ok("_abhed/mcp/restart", map[string]any{"sessionId": id, "name": "stub"}, &res)
	if res["status"] != "connected" {
		t.Fatalf("restart after the server came back: %v", res)
	}

	r.cl.refused(errParams, "_abhed/mcp/restart", map[string]any{"sessionId": id, "name": "nosuch"})
	r.cl.refused(errParams, "_abhed/mcp/restart", map[string]any{"sessionId": id})
	r.cl.refused(errParams, "_abhed/mcp/restart", map[string]any{"sessionId": "nosuch", "name": "stub"})

	got, actors := r.recorded(id, agent.EvMCPStatus)
	if len(got) != 3 || got[0]["status"] != "connected" || got[1]["status"] != "error" || got[2]["status"] != "connected" {
		t.Fatalf("mcp.status: %v", got)
	}
	for i, p := range got {
		if p["server"] != "stub" || p["op"] != "restart" || p["by"] != "user" || actors[i] != agent.ActorUser {
			t.Fatalf("mcp.status %d: %v by %v", i, p, actors[i])
		}
	}
}

// §6.4 a restart waits for the running prompt: the session's tools share the connection.
func TestStudioMCPRestartRefusedWhileAPromptRuns(t *testing.T) {
	srv, _ := mcpStub(t)
	r := newStudioRig(t, `,"mcp":{"servers":[{"name":"stub","url":"`+srv.URL+`","enabled":true}]}`)
	id := r.open()
	s := r.cl.conn.session(id)
	s.mu.Lock()
	s.cancel = func() {}
	s.mu.Unlock()
	r.cl.refused(errBusy, "_abhed/mcp/restart", map[string]any{"sessionId": id, "name": "stub"})
	s.mu.Lock()
	s.cancel = nil
	s.mu.Unlock()
	if got, _ := r.recorded(id, agent.EvMCPStatus); len(got) != 0 {
		t.Fatalf("a refused restart was recorded: %v", got)
	}
}

// TestStdioMCPHelper is an MCP server on stdin and stdout for the restart
// test: it writes its pid to the file ABHED_MCP_STDIO_PID names and answers
// until its input closes.
func TestStdioMCPHelper(t *testing.T) {
	pidFile := os.Getenv("ABHED_MCP_STDIO_PID")
	if pidFile == "" {
		t.Skip("run by TestStudioMCPRestartKeepsAStdioServerRunning")
	}
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600)
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		result := `{}`
		switch req.Method {
		case "initialize":
			result = `{"protocolVersion":"2024-11-05","serverInfo":{"name":"stdio","version":"1"},"capabilities":{"tools":{}}}`
		case "tools/list":
			result = `{"tools":[{"name":"lookup","inputSchema":{"type":"object"}}]}`
		}
		fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", req.ID, result)
	}
	os.Exit(0)
}

// §6.4 a restarted stdio server keeps running after the reply: its process
// lives as long as the session, not as long as the request that restarted it.
func TestStudioMCPRestartKeepsAStdioServerRunning(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	cfg, _ := json.Marshal(map[string]any{"servers": []any{map[string]any{"name": "stdio", "command": exe,
		"args": []string{"-test.run=^TestStdioMCPHelper$"}, "env": []string{"ABHED_MCP_STDIO_PID=" + pidFile}, "enabled": true}}})
	r := newStudioRig(t, `,"mcp":`+string(cfg))
	id := r.open()
	var res map[string]any
	r.cl.ok("_abhed/mcp/restart", map[string]any{"sessionId": id, "name": "stdio"}, &res)
	if res["status"] != "connected" {
		t.Fatalf("restart: %v", res)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatal("the helper server never started")
	}
	// After the reply the server must still answer: call its tool directly.
	time.Sleep(300 * time.Millisecond)
	tool, ok := r.cl.conn.session(id).parts.Set.Registry.Get("mcp__stdio__lookup")
	if !ok {
		t.Fatal("the server's tool is not registered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if res := tool.Run(ctx, nil, json.RawMessage(`{}`)); res.IsError {
		t.Fatalf("the restarted server no longer answers: %s", res.Content)
	}
	var caps map[string]any
	r.cl.ok("_abhed/capabilities", map[string]any{"sessionId": id}, &caps)
	if m := mcpEntry(t, caps, "stdio"); m["status"] != "connected" {
		t.Fatalf("after the restart: %v", m)
	}
}

// §6.4 a prompt waits for a restart: claimRestart holds the session.
func TestStudioMCPRestartHoldsTheSession(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	s := r.cl.conn.session(id)
	if e := claimRestart(s); e != nil {
		t.Fatal(e)
	}
	if e := claimRestart(s); e == nil || e.Code != errBusy {
		t.Fatalf("a second restart: %v", e)
	}
	if e := idle(s); e == nil || e.Code != errBusy {
		t.Fatalf("idle during a restart: %v", e)
	}
	r.cl.refused(errBusy, "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]any{"type": "text", "text": "hi"}}})
	s.mu.Lock()
	s.mcpRestart = false
	s.mu.Unlock()
}

// A restart's release acts once: called again after a later restart has
// claimed the session, it leaves that claim in place.
func TestStudioMCPRestartReleaseIsOneShot(t *testing.T) {
	r := newStudioRig(t, "")
	s := r.cl.conn.session(r.open())
	if e := claimRestart(s); e != nil {
		t.Fatal(e)
	}
	release := restartRelease(s)
	release()
	if e := claimRestart(s); e != nil {
		t.Fatalf("after release: %v", e)
	}
	release() // the first restart's deferred call
	if e := claimRestart(s); e == nil || e.Code != errBusy {
		t.Fatalf("the later restart's claim was cleared: %v", e)
	}
	restartRelease(s)()
}
