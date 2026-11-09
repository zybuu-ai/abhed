package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
)

// studioServers are the MCP servers in the rig's own ~/.abhed/config.json.
func studioServers(t *testing.T, home string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".abhed", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCP struct {
			Servers []map[string]any `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.MCP.Servers
}

// §6.4 the check describes a server, names its transport and the hash the
// command line is held to, and writes nothing; adding is not over ACP.
func TestStudioMCPCheck(t *testing.T) {
	r := newStudioRig(t, "")
	features := r.cl.conn.acpFeatures()
	if !strings.Contains(strings.Join(features, ","), "mcp.check") || strings.Contains(strings.Join(features, ","), "mcp.add") {
		t.Fatalf("features: %v", features)
	}
	id := r.open()
	srv := map[string]any{"sessionId": id, "name": "issues", "command": "python3", "args": []any{"server.py", "a b"}, "env": []any{"ISSUES_TOKEN"}, "allowTools": []any{"get_issue"}}
	var check struct {
		File, Description, Kind, SHA256 string
	}
	r.cl.ok("_abhed/mcp/check", srv, &check)
	s, _ := newMCPServer("issues", "python3", []string{"server.py", "a b"}, []string{"ISSUES_TOKEN"}, nil, []string{"get_issue"})
	_, sum := mcpEntrySum(s)
	if check.File != filepath.Join(r.home, ".abhed", "config.json") || !strings.Contains(check.Description, "python3 server.py 'a b'") ||
		!strings.Contains(check.Description, "ISSUES_TOKEN (your own value)") || check.Kind != "stdio" || check.SHA256 != sum {
		t.Fatalf("check: %+v", check)
	}
	if _, err := os.Stat(filepath.Join(r.home, ".abhed", "config-changes.jsonl")); err == nil {
		t.Fatal("check logged a change")
	}
	r.cl.refused(errNoMethod, "_abhed/mcp/add", srv)
}

// Removing narrows: strict about its fields, logged, and recorded with the
// entry's transport and hash.
func TestStudioMCPRemove(t *testing.T) {
	r := newStudioRig(t, `,"mcp":{"servers":[{"name":"issues","command":"python3","args":["server.py"],"enabled":true}]}`)
	id := r.open()
	r.cl.refused(errParams, "_abhed/mcp/remove", map[string]any{"sessionId": id, "name": "issues", "force": true})
	var removed map[string]any
	r.cl.ok("_abhed/mcp/remove", map[string]any{"sessionId": id, "name": "issues"}, &removed)
	if removed["removed"] != true || len(studioServers(t, r.home)) != 0 {
		t.Fatalf("remove: %v", removed)
	}
	r.cl.refused(errRefused, "_abhed/mcp/remove", map[string]any{"sessionId": id, "name": "issues"})
	evs, actors := r.recorded(id, agent.EvMCPStatus)
	if len(evs) != 1 || evs[0]["op"] != "remove" || evs[0]["kind"] != "stdio" || len(evs[0]["sha256"].(string)) != 64 || actors[0] != agent.ActorUser {
		t.Fatalf("mcp.status: %v %v", evs, actors)
	}
	log, _ := os.ReadFile(filepath.Join(r.home, ".abhed", "config-changes.jsonl"))
	if !strings.Contains(string(log), `"by":"studio mcp remove"`) || !strings.Contains(string(log), evs[0]["sha256"].(string)) {
		t.Fatalf("change log: %s", log)
	}
}

// A bad server, a value in env, an unknown field or session is refused.
func TestStudioMCPCheckRefusesBadInput(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	for _, p := range []map[string]any{
		{"sessionId": id, "name": "x", "command": "srv", "env": []any{"TOKEN=sk-live-123"}},
		{"sessionId": id, "name": "bad name", "command": "srv"},
		{"sessionId": id, "name": "x"},
		{"sessionId": id, "name": "x", "command": "srv", "url": "https://mcp.example.com"},
		{"sessionId": id, "name": "x", "command": "https://mcp.example.com"},
		{"sessionId": id, "name": "x", "url": "https://mcp.example.com", "headersEnv": map[string]any{"Authorization": "not a var"}},
		{"sessionId": id, "name": "x", "command": "srv", "headersEnv": map[string]any{"Authorization": "TOKEN"}},
		{"sessionId": id, "name": "x", "command": "srv", "secret": "y"},
		{"sessionId": "nosuch", "name": "x", "command": "srv"},
	} {
		r.cl.refused(errParams, "_abhed/mcp/check", p)
	}
}

// The managed lists and an agent's command refuse at the check, and the
// refused attempt is recorded with who made it.
func TestStudioMCPCheckUnderPolicy(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	path := filepath.Join(t.TempDir(), "managed.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"denied":[{"host":"paste.example.com"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	managed.ConfigFile = path // acpModelsEnv restores it
	r.cl.refused(errPolicy, "_abhed/mcp/check", map[string]any{"sessionId": id, "name": "pastebin", "url": "https://paste.example.com/mcp"})
	r.cl.ok("_abhed/mcp/check", map[string]any{"sessionId": id, "name": "issues", "url": "https://issues.example.com/mcp", "headersEnv": map[string]any{"Authorization": "ISSUES_AUTH"}}, nil)
	old := inAgentCommand
	t.Cleanup(func() { inAgentCommand = old })
	inAgentCommand = func() string { return "ABHED_SANDBOX is set" }
	r.cl.refused(errPolicy, "_abhed/mcp/check", map[string]any{"sessionId": id, "name": "other", "command": "srv"})
	r.cl.refused(errPolicy, "_abhed/mcp/remove", map[string]any{"sessionId": id, "name": "issues"})
	evs, _ := r.recorded(id, agent.EvConfigRefused)
	var mcp []map[string]any
	for _, e := range evs {
		if e["key"] == "mcp.servers" {
			mcp = append(mcp, e)
		}
	}
	if len(mcp) != 2 || !strings.HasPrefix(mcp[0]["value"].(string), "pastebin sha256:") || !strings.Contains(mcp[0]["reason"].(string), "mcp.denied") ||
		!strings.Contains(mcp[1]["reason"].(string), "agent's command") {
		t.Fatalf("config.refused: %v", mcp)
	}
}

// An add made by the engine's command line (as Studio runs it) reaches the
// next session's hash-chained record as config.changed, once.
func TestStudioMCPAddReachesTheNextSessionsRecord(t *testing.T) {
	r := newStudioRig(t, "")
	s, _ := newMCPServer("issues", "python3", []string{"server.py"}, nil, nil, nil)
	if _, err := writeMCPServer(s, "mcp add -confirm-fd"); err != nil {
		t.Fatal(err)
	}
	id := r.open()
	evs, _ := r.recorded(id, agent.EvConfigChanged)
	_, sum := mcpEntrySum(s)
	if len(evs) != 1 || evs[0]["by"] != "mcp add -confirm-fd" || evs[0]["name"] != "issues" || evs[0]["sha256"] != sum {
		t.Fatalf("config.changed: %v", evs)
	}
	if evs, _ := r.recorded(r.open(), agent.EvConfigChanged); len(evs) != 0 {
		t.Fatalf("recorded again: %v", evs)
	}
}
