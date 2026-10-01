package app

import (
	"encoding/json"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
)

// §5.1 modes: what the engine allows now, changed through the engine's
// ceiling, recorded mode.changed by the person, via studio.
func TestStudioModes(t *testing.T) {
	r := newStudioRig(t, "")
	var res struct {
		SessionID string `json:"sessionId"`
		Modes     struct {
			CurrentModeID  string           `json:"currentModeId"`
			AvailableModes []map[string]any `json:"availableModes"`
		} `json:"modes"`
	}
	r.cl.ok("session/new", map[string]any{"cwd": r.ws, "mcpServers": []any{}}, &res)
	var ids []string
	for _, m := range res.Modes.AvailableModes {
		ids = append(ids, m["id"].(string))
	}
	if res.Modes.CurrentModeID != "default" || strings.Join(ids, ",") != "default,accept-edits,plan,auto" {
		t.Fatalf("modes: %s %v", res.Modes.CurrentModeID, ids)
	}
	from := r.cl.mark()
	r.cl.ok("session/set_mode", map[string]any{"sessionId": res.SessionID, "modeId": "plan"}, nil)
	r.cl.waitFor(from, "current_mode_update", func(m rpcMessage) bool {
		return strings.Contains(string(m.Params), `"current_mode_update"`) && strings.Contains(string(m.Params), `"plan"`)
	})
	changed, actors := r.recorded(res.SessionID, agent.EvModeChanged)
	if len(changed) != 1 || changed[0]["to"] != "plan" || changed[0]["by"] != "user" || changed[0]["via"] != "studio" || actors[0] != agent.ActorUser {
		t.Fatalf("mode.changed: %v %v", changed, actors)
	}
	// Bypass is not offered unless the person's own configuration chose it.
	r.cl.refused(errPolicy, "session/set_mode", map[string]any{"sessionId": res.SessionID, "modeId": "bypass"})
	r.cl.refused(errPolicy, "session/set_mode", map[string]any{"sessionId": res.SessionID, "modeId": "yolo"})
	// The same list is a config option of category mode.
	var opts struct {
		ConfigOptions []map[string]any `json:"configOptions"`
	}
	r.cl.ok("session/set_config_option", map[string]any{"sessionId": res.SessionID, "configId": "mode", "value": "default"}, &opts)
	found := false
	for _, o := range opts.ConfigOptions {
		found = found || (o["category"] == "mode" && o["currentValue"] == "default")
	}
	if !found {
		t.Fatalf("config options after a mode change: %v", opts.ConfigOptions)
	}
}

// §5.1 a managed ceiling holds over ACP whatever Studio asked.
func TestStudioManagedModeCeiling(t *testing.T) {
	r := newStudioRig(t, "")
	path := filepath.Join(t.TempDir(), "managed.json")
	if err := os.WriteFile(path, []byte(`{"permissions":{"mode":"default"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	managed.ConfigFile = path
	var res struct {
		SessionID string `json:"sessionId"`
		Modes     struct {
			AvailableModes []map[string]any `json:"availableModes"`
		} `json:"modes"`
	}
	r.cl.ok("session/new", map[string]any{"cwd": r.ws, "mcpServers": []any{}}, &res)
	for _, m := range res.Modes.AvailableModes {
		if id := m["id"]; id != "default" && id != "plan" {
			t.Fatalf("mode %v offered under a managed default", id)
		}
		if meta(m)["locked"] != true {
			t.Fatalf("not marked locked: %v", m)
		}
	}
	r.cl.refused(errPolicy, "session/set_mode", map[string]any{"sessionId": res.SessionID, "modeId": "auto"})
	r.cl.refused(errPolicy, "session/set_mode", map[string]any{"sessionId": res.SessionID, "modeId": "accept-edits"})
}

// §5.4 explain is a dry run: the rule that decides, redacted, nothing recorded.
func TestStudioExplainAndPolicyView(t *testing.T) {
	r := newStudioRig(t, `,"permissions":{"deny":["bash(curl *)"],"ask":["bash(gh *)"]}`)
	id := r.open()
	before := len(r.events(id))
	var d map[string]any
	r.cl.ok("_abhed/policy/explain", map[string]any{"sessionId": id, "tool": "bash", "args": map[string]any{"command": "curl example.com"}}, &d)
	if d["decision"] != "deny" || d["rule"] != "bash(curl *)" || d["step"] != "deny" {
		t.Fatalf("explain: %v", d)
	}
	r.cl.ok("_abhed/policy/explain", map[string]any{"sessionId": id, "tool": "bash", "args": map[string]any{"command": "rm -rf /tmp/x"}}, &d)
	if d["decision"] != "ask" || d["step"] != "destructive" || d["scope"] != nil {
		t.Fatalf("explain destructive: %v", d)
	}
	if after := len(r.events(id)); after != before {
		t.Fatalf("explain was recorded: %d events, then %d", before, after)
	}
	var caps map[string]any
	r.cl.ok("_abhed/capabilities", map[string]any{"sessionId": id}, &caps)
	pol := caps["policy"].(map[string]any)
	var rules []string
	for _, x := range pol["rules"].([]any) {
		x := x.(map[string]any)
		rules = append(rules, x["decision"].(string)+":"+x["rule"].(string)+":"+x["layer"].(string))
	}
	joined := strings.Join(rules, " ")
	if !strings.Contains(joined, "deny:bash(curl *):user") || !strings.Contains(joined, "ask:builtin:destructive:builtin") {
		t.Fatalf("policy rules: %v", rules)
	}
	if caps["studio"].(map[string]any)["host_terminal"] != true || caps["memory_auto"] != false {
		t.Fatalf("capabilities: %v", caps)
	}
}

// §5.4 a workspace file not trusted shows its rules as not applied.
func TestStudioPolicyViewShowsIgnoredWorkspaceRules(t *testing.T) {
	r := newStudioRig(t, "")
	r.write(".abhed/config.json", `{"permissions":{"allow":["bash(*)"]}}`)
	id := r.open()
	var caps map[string]any
	r.cl.ok("_abhed/capabilities", map[string]any{"sessionId": id}, &caps)
	for _, x := range caps["policy"].(map[string]any)["rules"].([]any) {
		x := x.(map[string]any)
		if x["rule"] == "bash(*)" {
			if x["applied"] != false || x["ignoredBecause"] != "workspace-untrusted" || x["layer"] != "workspace" {
				t.Fatalf("ignored rule: %v", x)
			}
			return
		}
	}
	t.Fatalf("the ignored allow rule is not shown: %v", caps["policy"])
}

// §5.5 an edit's ask carries its diff, and the "why" line's rule.
func TestStudioAskCarriesDiff(t *testing.T) {
	r := newStudioRig(t, "")
	file := r.write("a.txt", "one\ntwo\nthree\n")
	r.model.script(callTool("c1", "read", map[string]any{"path": file}),
		callTool("c2", "edit", map[string]any{"path": file, "old_string": "two", "new_string": "TWO"}), say("done"))
	var asked json.RawMessage
	r.cl.answering(func(method string, params json.RawMessage) any {
		if method == "session/request_permission" {
			asked = params
			return chosen(params, "allow_once")
		}
		return map[string]any{}
	})
	id := r.open()
	_, ups := r.prompt(id, "edit it")
	if asked == nil {
		t.Fatalf("no ask: %v", ups)
	}
	var p struct {
		ToolCall struct {
			Content []map[string]any `json:"content"`
			Meta    map[string]struct {
				Rule string `json:"rule"`
				Diff []struct {
					OldText string `json:"oldText"`
					NewText string `json:"newText"`
				} `json:"diff"`
			} `json:"_meta"`
		} `json:"toolCall"`
	}
	_ = json.Unmarshal(asked, &p)
	m := p.ToolCall.Meta[acpMetaKey]
	if len(p.ToolCall.Content) != 1 || p.ToolCall.Content[0]["type"] != "diff" || len(m.Diff) != 1 ||
		m.Diff[0].OldText != "one\ntwo\nthree\n" || m.Diff[0].NewText != "one\nTWO\nthree\n" || m.Rule != "builtin:default" {
		t.Fatalf("ask: %s", asked)
	}
	if r.read("a.txt") != "one\nTWO\nthree\n" {
		t.Fatal("the allowed edit was not made")
	}
}

// §5.6 inspect shows what the workspace file would bring; a change to its
// bytes during a session is told to Studio. There is no grant method.
func TestStudioTrustInspectAndChange(t *testing.T) {
	r := newStudioRig(t, "", say("ok"), say("ok"))
	r.write(".abhed/config.json", `{"mcp":{"servers":[{"name":"wsmcp","command":"/bin/true","enabled":true}]}}`)
	r.write(".abhed/commands/hello.md", "say hello")
	var st map[string]any
	r.cl.ok("_abhed/trust/inspect", map[string]any{"cwd": r.ws}, &st)
	if st["trusted"] != false || len(st["mcp"].([]any)) != 1 || len(st["commands"].([]any)) != 1 {
		t.Fatalf("inspect: %v", st)
	}
	r.cl.refused(errNoMethod, "_abhed/trust/grant", map[string]any{"cwd": r.ws})
	id := r.open()
	r.write(".abhed/config.json", `{"permissions":{"deny":["bash(*)"]}}`)
	from := r.cl.mark()
	r.prompt(id, "hi")
	r.cl.waitFor(from, "_abhed/trust/changed", func(m rpcMessage) bool { return m.Method == "_abhed/trust/changed" })
}

// §2.4: a stored secret's value never reaches Studio: not in an ask's diff,
// not in explain, and the capabilities list secrets by name.
func TestRuleNoSecretValueReachesStudio(t *testing.T) {
	r := newStudioRig(t, "")
	const value = "sk-live-0123456789abcdef"
	if err := secrets.Default().Set("API_TOKEN", value); err != nil {
		t.Fatal(err)
	}
	file := r.write("app.env", "TOKEN="+value+"\nMODE=dev\n")
	r.model.script(callTool("c1", "read", map[string]any{"path": file}),
		callTool("c2", "edit", map[string]any{"path": file, "old_string": "MODE=dev", "new_string": "MODE=prod"}), say("done"))
	var asked []string
	r.cl.answering(func(method string, params json.RawMessage) any {
		asked = append(asked, string(params))
		return chosen(params, "reject_once")
	})
	id := r.open()
	r.prompt(id, "flip the mode")
	if len(asked) == 0 {
		t.Fatal("no ask")
	}
	for _, a := range asked {
		if strings.Contains(a, value) {
			t.Fatalf("the ask carried the secret: %s", a)
		}
	}
	var caps map[string]any
	m := r.cl.call("_abhed/capabilities", map[string]any{"sessionId": id})
	if strings.Contains(string(m.Result), value) {
		t.Fatal("capabilities carried the secret")
	}
	_ = json.Unmarshal(m.Result, &caps)
	if names := caps["secrets"].([]any); len(names) != 1 || names[0].(map[string]any)["name"] != "API_TOKEN" {
		t.Fatalf("secrets: %v", caps["secrets"])
	}
	e := r.cl.call("_abhed/policy/explain", map[string]any{"sessionId": id, "tool": "bash", "args": map[string]any{"command": "echo " + value}})
	if strings.Contains(string(e.Result), value) {
		t.Fatalf("explain carried the secret: %s", e.Result)
	}
}
