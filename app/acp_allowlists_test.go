package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
)

// Under a managed model.allowed the editor is offered only the admitted
// models; a switch to another is refused with the reason and recorded, and
// the policy view shows the list.
func TestACPAllowedModels(t *testing.T) {
	a1, b1 := newACPModelStub(t, "a", false), newACPModelStub(t, "b", false)
	agents := acpModelsEnv(t, `{"model":{"default":"alpha","providers":{"alpha":`+stubProvider(a1.URL, "m-a")+
		`,"beta":`+stubProvider(b1.URL, "m-b")+`}}}`)
	path := filepath.Join(t.TempDir(), "managed.json")
	if err := os.WriteFile(path, []byte(`{"model":{"allowed":["openai-compatible/m-a"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	managed.ConfigFile = path // acpModelsEnv restores it
	cl := newACPClient(t, nil)
	s := acpOpen(t, cl, t.TempDir())
	if current, listed := selectedModels(t, s.ConfigOptions); current != "alpha" || fmt.Sprint(listed) != "[alpha=alpha=m-a (openai-compatible)]" {
		t.Fatalf("offered: %q %v", current, listed)
	}
	for i, req := range []struct {
		method string
		params map[string]any
	}{
		{"session/set_config_option", map[string]any{"sessionId": s.SessionID, "configId": "model", "value": "beta"}},
		{"session/set_model", map[string]any{"sessionId": s.SessionID, "modelId": "beta"}},
	} {
		res := cl.request(10+i, req.method, req.params)
		if res.Error == nil || res.Error.Code != errRefused || !strings.Contains(res.Error.Message, "model.allowed") ||
			!strings.Contains(res.Error.Message, "openai-compatible/m-b") {
			t.Fatalf("%s: %s %+v", req.method, res.Result, res.Error)
		}
	}
	a := agents()[0]
	if got := switchedTo(a); len(got) != 0 {
		t.Fatalf("a refused switch was made: %v", got)
	}
	refusals := 0
	for _, ev := range a.Events() {
		if ev.Type == agent.EvConfigRefused {
			var c agent.ConfigAttempt
			_ = json.Unmarshal(ev.Payload, &c)
			if c.Key == "model.allowed" && c.Decision == "refused" {
				refusals++
			}
		}
	}
	if refusals != 2 {
		t.Fatalf("refusals recorded: %d", refusals)
	}
}

// A client's own MCP servers in session/new are not started, and the reason
// is given and recorded.
func TestACPSessionNewMCPServersRefusedWithReason(t *testing.T) {
	a1 := newACPModelStub(t, "a", false)
	agents := acpModelsEnv(t, `{"model":{"default":"alpha","providers":{"alpha":`+stubProvider(a1.URL, "m-a")+`}}}`)
	cl := newACPClient(t, nil)
	cl.request(1, "initialize", map[string]any{"protocolVersion": 1})
	res := cl.request(2, "session/new", map[string]any{"cwd": t.TempDir(),
		"mcpServers": []any{map[string]any{"name": "theirs", "command": "/bin/sh", "args": []string{}, "env": []any{}}}})
	if res.Error != nil {
		t.Fatal(res.Error.Message)
	}
	var out struct {
		Meta map[string]map[string]any `json:"_meta"`
	}
	_ = json.Unmarshal(res.Result, &out)
	block := out.Meta[acpMetaKey]
	if fmt.Sprint(block["mcpServersRefused"]) != "[theirs]" || block["mcpServersRefusedReason"] != acpMCPRefused {
		t.Fatalf("meta: %v", block)
	}
	found := false
	for _, ev := range agents()[0].Events() {
		if ev.Type == agent.EvConfigRefused && strings.Contains(string(ev.Payload), `"mcp.servers"`) && strings.Contains(string(ev.Payload), "theirs") {
			found = true
		}
	}
	if !found {
		t.Fatal("the refused server was not recorded")
	}
}

// A server the managed MCP lists refuse is never started; Studio is told it
// is refused and why, and the policy view shows the lists.
func TestStudioMCPListsRefuseAServer(t *testing.T) {
	srv, _ := mcpStub(t)
	r := newStudioRig(t, `,"mcp":{"servers":[{"name":"stub","url":"`+srv.URL+`","enabled":true},`+
		`{"name":"other","url":"`+srv.URL+`/x","enabled":true}]}`)
	path := filepath.Join(t.TempDir(), "managed.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"allowed":[{"name":"stub","host":"127.0.0.1"}]},"model":{"allowed":["openai-compatible/*"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	managed.ConfigFile = path // acpModelsEnv restores it
	id := r.open()
	var caps map[string]any
	r.cl.ok("_abhed/capabilities", map[string]any{"sessionId": id}, &caps)
	if m := mcpEntry(t, caps, "stub"); m["status"] != "connected" {
		t.Fatalf("admitted: %v", m)
	}
	other := mcpEntry(t, caps, "other")
	if other["status"] != "refused" || !strings.Contains(fmt.Sprint(other["error"]), "mcp.allowed") {
		t.Fatalf("refused: %v", other)
	}
	pol, _ := caps["policy"].(map[string]any)
	lists := fmt.Sprint(pol["allowLists"])
	if !strings.Contains(lists, "model.allowed") || !strings.Contains(lists, "mcp.allowed") || !strings.Contains(lists, "managed") {
		t.Fatalf("policy view: %v", pol["allowLists"])
	}
	found := false
	for _, ev := range r.events(id) {
		if ev.Type == agent.EvConfigRefused && strings.Contains(string(ev.Payload), `"mcp.servers"`) && strings.Contains(string(ev.Payload), `"other"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("the refused server was not recorded")
	}
}

// Completions, next-edit suggestions and commit messages hold the allowed
// models: a model outside them is refused with the reason.
func TestEditorModelsHoldTheAllowedModels(t *testing.T) {
	fim := newFIMServer(t, "p")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	path := filepath.Join(t.TempDir(), "managed.json")
	if err := os.WriteFile(path, []byte(`{"model":{"allowed":["openai-compatible/m-*"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	managed.ConfigFile = path // acpModelsEnv restores it
	id := r.open()
	var res completeItems
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "x"}, &res)
	if !strings.Contains(res.Disabled, "model.allowed") || len(res.Items) != 0 {
		t.Fatalf("completion: %+v", res)
	}
	cfg := r.cl.conn.session(id).parts.Config
	for _, feature := range []string{"next_edit", "commit"} {
		if why, _ := editorOff(cfg, feature); !strings.Contains(why, "model.allowed") {
			t.Errorf("%s: %q", feature, why)
		}
	}
}
