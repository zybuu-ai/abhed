package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

// §6.4 Studio adds a server through the engine: checked, written to the
// person's own file with the change log, recorded, and removable.
func TestStudioMCPAddAndRemove(t *testing.T) {
	r := newStudioRig(t, "")
	if !slices.Contains(r.cl.conn.acpFeatures(), "mcp.add") {
		t.Fatalf("features: %v", r.cl.conn.acpFeatures())
	}
	id := r.open()
	srv := map[string]any{"sessionId": id, "name": "issues", "command": "python3", "args": []any{"server.py"}, "env": []any{"ISSUES_TOKEN"}, "allowTools": []any{"get_issue"}}
	var check struct {
		File        string `json:"file"`
		Description string `json:"description"`
	}
	r.cl.ok("_abhed/mcp/check", srv, &check)
	if check.File != filepath.Join(r.home, ".abhed", "config.json") || !strings.Contains(check.Description, "python3 server.py") || !strings.Contains(check.Description, "ISSUES_TOKEN (your own value)") {
		t.Fatalf("check: %+v", check)
	}
	if got := studioServers(t, r.home); len(got) != 0 {
		t.Fatalf("check wrote: %v", got)
	}
	var added map[string]any
	r.cl.ok("_abhed/mcp/add", srv, &added)
	got := studioServers(t, r.home)
	if added["added"] != true || len(got) != 1 || got[0]["name"] != "issues" || got[0]["command"] != "python3" || got[0]["enabled"] != true {
		t.Fatalf("add: %v %v", added, got)
	}
	log, err := os.ReadFile(filepath.Join(r.home, ".abhed", "config-changes.jsonl"))
	if err != nil || !strings.Contains(string(log), `"by":"studio mcp add"`) {
		t.Fatalf("change log: %s %v", log, err)
	}
	// The same name again is refused, and the file keeps one entry.
	r.cl.refused(errRefused, "_abhed/mcp/check", srv)
	r.cl.refused(errRefused, "_abhed/mcp/add", srv)

	var removed map[string]any
	r.cl.ok("_abhed/mcp/remove", map[string]any{"sessionId": id, "name": "issues"}, &removed)
	if removed["removed"] != true || len(studioServers(t, r.home)) != 0 {
		t.Fatalf("remove: %v", removed)
	}
	r.cl.refused(errRefused, "_abhed/mcp/remove", map[string]any{"sessionId": id, "name": "issues"})

	evs, actors := r.recorded(id, agent.EvMCPStatus)
	if len(evs) != 2 || evs[0]["op"] != "add" || evs[1]["op"] != "remove" || evs[0]["server"] != "issues" || actors[0] != agent.ActorUser || evs[0]["by"] != "user" {
		t.Fatalf("mcp.status: %v %v", evs, actors)
	}
}

// A value is never written; a bad server, an unknown field or an unknown
// session is refused before anything is.
func TestStudioMCPAddRefusesBadInput(t *testing.T) {
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
	} {
		r.cl.refused(errParams, "_abhed/mcp/add", p)
	}
	r.cl.refused(errParams, "_abhed/mcp/add", map[string]any{"sessionId": "nosuch", "name": "x", "command": "srv"})
	if _, err := os.Stat(filepath.Join(r.home, ".abhed", "config.json")); err == nil {
		if got := studioServers(t, r.home); len(got) != 0 {
			t.Fatalf("written: %v", got)
		}
	}
}

// The managed configuration's MCP lists and its servers still decide, and an
// agent's command cannot add one.
func TestStudioMCPAddUnderPolicy(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	path := filepath.Join(t.TempDir(), "managed.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"denied":[{"host":"paste.example.com"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	managed.ConfigFile = path // acpModelsEnv restores it
	r.cl.refused(errPolicy, "_abhed/mcp/check", map[string]any{"sessionId": id, "name": "pastebin", "url": "https://paste.example.com/mcp"})
	r.cl.refused(errPolicy, "_abhed/mcp/add", map[string]any{"sessionId": id, "name": "pastebin", "url": "https://paste.example.com/mcp"})
	r.cl.ok("_abhed/mcp/add", map[string]any{"sessionId": id, "name": "issues", "url": "https://issues.example.com/mcp", "headersEnv": map[string]any{"Authorization": "ISSUES_AUTH"}}, nil)

	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.cl.refused(errPolicy, "_abhed/mcp/add", map[string]any{"sessionId": id, "name": "other", "command": "srv"})
	r.cl.refused(errPolicy, "_abhed/mcp/remove", map[string]any{"sessionId": id, "name": "issues"})

	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := inAgentCommand
	t.Cleanup(func() { inAgentCommand = old })
	inAgentCommand = func() string { return "ABHED_SANDBOX is set" }
	r.cl.refused(errPolicy, "_abhed/mcp/add", map[string]any{"sessionId": id, "name": "other", "command": "srv"})
	r.cl.refused(errPolicy, "_abhed/mcp/remove", map[string]any{"sessionId": id, "name": "issues"})
	if got := studioServers(t, r.home); len(got) != 1 || got[0]["name"] != "issues" {
		t.Fatalf("servers: %v", got)
	}
}
