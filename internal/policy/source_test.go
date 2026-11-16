package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every decision says where it came from: the rule's layer as RuleSource
// names it, a session rule, an edition's layer, the mode or a hook.
func TestResultSource(t *testing.T) {
	bash := func(cmd string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"command": cmd})
		return b
	}
	e := New(ModeDefault)
	e.RuleSource = func(list, rule string) (string, string) {
		if rule == "bash(curl*)" && list == "deny" {
			return "org", "the org policy (https://org.example, version 1)"
		}
		return "user", ""
	}
	if err := e.AddDeny("bash(curl*)"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAllow("bash(ls*)"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddLayer("team", []string{"bash(rm -rf /*)"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	e.Session = &Overlay{}
	if _, err := e.Session.Add("deny", "bash(scp *)"); err != nil {
		t.Fatal(err)
	}
	res := e.Evaluate("bash", true, bash("curl -sI https://example.com"))
	if res.Source != "org" || res.Reason != "denied by rule bash(curl*) from the org policy (https://org.example, version 1)" {
		t.Fatalf("org rule: %+v", res)
	}
	if res := e.Evaluate("bash", true, bash("ls -l")); res.Source != "user" || strings.Contains(res.Reason, " from ") {
		t.Fatalf("a layer with no words adds none: %+v", res)
	}
	if res := e.Evaluate("bash", true, bash("rm -rf /tmp")); res.Source != "team" {
		t.Fatalf("edition layer: %+v", res)
	}
	if res := e.Evaluate("bash", true, bash("scp a b:")); res.Source != "session" {
		t.Fatalf("session rule: %+v", res)
	}
	if res := e.Evaluate("bash", true, bash("make")); res.Source != "mode" || res.Step != "default" {
		t.Fatalf("default step: %+v", res)
	}
	e.Mode = ModePlan
	if res := e.Evaluate("write", true, json.RawMessage(`{"path":"a"}`)); res.Source != "mode" || res.Decision != Deny {
		t.Fatalf("plan mode: %+v", res)
	}
	e.Hooks = append(e.Hooks, func(string, json.RawMessage) *Result {
		return &Result{Decision: Deny, Reason: "the hook says no"}
	})
	if res := e.Evaluate("bash", true, bash("ls")); res.Source != "hook" {
		t.Fatalf("hook: %+v", res)
	}
}
