package policy

import (
	"encoding/json"
	"testing"
)

// A built-in tool name in any case names the tool: Bash(curl *) used to
// match no tool, so the deny rule never fired.
func TestDenyRulesNameBuiltinToolsInAnyCase(t *testing.T) {
	e := New(ModeBypass)
	if err := e.AddDeny("Bash(curl *)", "BASH(wget*)", "Read(/etc/shadow)", " Write (*.pem)"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		tool string
		args json.RawMessage
	}{
		{"bash", cmd("curl https://example.com")},
		{"bash", cmd("wget -q x")},
		{"bash", cmd("echo ok && curl x")},
		{"read", pathArg("/etc/shadow")},
		{"write", pathArg("key.pem")},
	} {
		if d := e.Evaluate(c.tool, true, c.args); d.Decision != Deny {
			t.Errorf("%s %s: %s (%s), want deny", c.tool, c.args, d.Decision, d.Reason)
		}
	}
	if d := e.EvaluateLine("bash", true, cmd("curl x")); d.Decision != Deny {
		t.Errorf("terminal line: %s", d.Decision)
	}
	if !e.Screens("bash") {
		t.Error("an interactive shell is not screened under Bash(curl *)")
	}
	// The reason names the rule as it was written.
	if d := e.Evaluate("bash", true, cmd("curl x")); d.Rule != "Bash(curl *)" {
		t.Errorf("rule %q, want it as written", d.Rule)
	}
}

// An allow rule folds as well, so it allows what its author meant, and
// nothing else: other commands, other tools and look-alike names are untouched.
func TestAllowRulesFoldOnlyToTheToolNamed(t *testing.T) {
	e := New(ModeDefault)
	if err := e.AddAllow("Bash(go test*)", "Read"); err != nil {
		t.Fatal(err)
	}
	if d := e.Evaluate("bash", true, cmd("go test ./...")); d.Decision != Allow {
		t.Errorf("go test: %s, want allow", d.Decision)
	}
	if d := e.Evaluate("bash", true, cmd("rm -f x")); d.Decision == Allow {
		t.Error("Bash(go test*) allowed another command")
	}
	if d := e.Evaluate("mcp__fs__read", true, pathArg("/x")); d.Decision == Allow {
		t.Error("Read allowed an MCP tool")
	}
	if d := e.Evaluate("edit", true, pathArg("/x")); d.Decision == Allow {
		t.Error("Read allowed edit")
	}
	for _, s := range []string{"Bash", "bASH(x)", "WEB_FETCH(https://a/*)", "k8s_GET"} {
		r, err := ParseRule(s)
		if err != nil || !BuiltinTool(r.Tool()) {
			t.Errorf("%s: tool %q err %v", s, r.Tool(), err)
		}
	}
	// A non-ASCII look-alike is no built-in name.
	if r, _ := ParseRule("K8s_get"); r.Tool() == "k8s_get" {
		t.Error("the Kelvin sign was read as k")
	}
}

// MCP and extension tool names are compared exactly: their protocols tell
// one case from another, so a rule is never moved onto another tool.
func TestMCPToolNamesAreNotFolded(t *testing.T) {
	e := New(ModeBypass)
	if err := e.AddDeny("mcp__GitHub__create_issue"); err != nil {
		t.Fatal(err)
	}
	if d := e.Evaluate("mcp__GitHub__create_issue", true, json.RawMessage(`{}`)); d.Decision != Deny {
		t.Errorf("exact name: %s", d.Decision)
	}
	if d := e.Evaluate("mcp__github__create_issue", true, json.RawMessage(`{}`)); d.Decision == Deny {
		t.Error("a rule for server GitHub was applied to server github")
	}
	for _, n := range []string{"mcp__x__bash", "Jira", "rag_Docs"} {
		if CanonicalTool(n) != n {
			t.Errorf("%s was folded to %s", n, CanonicalTool(n))
		}
	}
	if !ReservedToolName("Bash") || !ReservedToolName("bash") || ReservedToolName("bash2") {
		t.Error("reserved names")
	}
}

// A session rule in another case is the same rule: added once, removed by either spelling.
func TestSessionRulesCompareTheirCanonicalForm(t *testing.T) {
	var o Overlay
	if added, _ := o.Add(ListDeny, "bash(curl *)"); !added {
		t.Fatal("not added")
	}
	if added, _ := o.Add(ListDeny, "Bash(curl *)"); added {
		t.Error("the same rule in another case was added twice")
	}
	if removed, _ := o.Remove(ListDeny, "BASH(curl *)"); !removed {
		t.Error("not removed by another spelling")
	}
}

func TestSuggestTool(t *testing.T) {
	for in, want := range map[string]string{"WebFetch": "web_fetch", "MultiEdit": "edit", "bsh": "bash", "zzzzzzzz": "",
		"sql": "", "git": "", "sed": "", "writ": "write", "secrets": "secret", "taskz": "task", "tests": "tasks"} {
		if got := SuggestTool(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

// mcp__server__* names every tool of one server, as agent definitions teach;
// a deny rule written so used to match no tool and allowed them all.
func TestMCPServerWildcardRules(t *testing.T) {
	e := New(ModeBypass)
	if err := e.AddDeny("mcp__github__*", "mcp__fs__x (*)"); err != nil {
		t.Fatal(err)
	}
	none := json.RawMessage(`{}`)
	for _, tool := range []string{"mcp__github__create_issue", "mcp__github__a__b", "mcp__fs__x"} {
		if d := e.Evaluate(tool, true, none); d.Decision != Deny {
			t.Errorf("%s: %s, want deny", tool, d.Decision)
		}
	}
	for _, tool := range []string{"mcp__githubx__a", "mcp__GitHub__create_issue", "mcp__github__", "bash"} {
		if d := e.Evaluate(tool, true, cmd("ls")); d.Decision == Deny {
			t.Errorf("%s was denied by a rule for another server", tool)
		}
	}
	a := New(ModeDefault)
	if err := a.AddAllow("mcp__github__*"); err != nil {
		t.Fatal(err)
	}
	if d := a.Evaluate("mcp__github__list", true, none); d.Decision != Allow {
		t.Errorf("allow wildcard: %s", d.Decision)
	}
	if d := a.Evaluate("mcp__gitlab__list", true, none); d.Decision == Allow {
		t.Error("allow wildcard reached another server")
	}
	for name, want := range map[string]bool{"mcp__github__*": true, "mcp__a-b_c__*": true, "mcp____*": false,
		"mcp__a b__*": false, "mcp__github__x*": false, "MCP__github__*": false} {
		if _, ok := MCPServerWildcard(name); ok != want {
			t.Errorf("%s: %v, want %v", name, ok, want)
		}
	}
}
