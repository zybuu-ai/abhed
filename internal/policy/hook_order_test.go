package policy

import (
	"encoding/json"
	"testing"
)

// A hook's allow is no opinion, and its ask waits for the deny rules and plan
// mode, so a hook can never turn a refusal into a question.
func TestHookCannotLoosenADecision(t *testing.T) {
	allow := func(string, json.RawMessage) *Result { return &Result{Decision: Allow, Reason: "a hook says yes"} }
	ask := func(string, json.RawMessage) *Result { return &Result{Decision: Ask, Reason: "a hook asks"} }
	bash := func(c string) json.RawMessage { return args(map[string]string{"command": c}) }

	e := New(ModeDefault)
	if err := e.AddDeny("bash(curl *)"); err != nil {
		t.Fatal(err)
	}
	e.Hooks = []Hook{allow}
	if got := e.Evaluate("bash", true, bash("curl x")); got.Decision != Deny {
		t.Fatalf("a hook's allow lifted a deny: %+v", got)
	}
	if got := e.Evaluate("bash", true, bash("make")); got.Decision != Ask {
		t.Fatalf("a hook's allow approved a call that asks: %+v", got)
	}
	e.Hooks = []Hook{allow, ask}
	if got := e.Evaluate("bash", true, bash("curl x")); got.Decision != Deny {
		t.Fatalf("a hook's ask turned a deny into a question: %+v", got)
	}
	if got := e.Evaluate("bash", true, bash("ls")); got.Decision != Ask || got.Step != "hook" {
		t.Fatalf("a hook's ask was lost: %+v", got)
	}

	p := New(ModePlan)
	p.Hooks = []Hook{ask}
	if got := p.Evaluate("bash", true, bash("make")); got.Decision != Deny || got.Step != "mode" {
		t.Fatalf("a hook's ask put a plan-mode change to the person: %+v", got)
	}

	// A destructive command or an ask rule keeps its own, stronger reason.
	if err := e.AddAsk("bash(make *)"); err != nil {
		t.Fatal(err)
	}
	if got := e.Evaluate("bash", true, bash("rm -rf /tmp/x")); got.Decision != Ask || got.Step != "destructive" {
		t.Fatalf("a hook's ask hid the destructive reason: %+v", got)
	}
	if got := e.Evaluate("bash", true, bash("make all")); got.Decision != Ask || got.Step != "ask" {
		t.Fatalf("a hook's ask hid the ask rule: %+v", got)
	}
	// No mode skips a hook's ask, not even bypass or an allow rule.
	for _, m := range []Mode{ModeAcceptEdits, ModeBypass} {
		b := New(m)
		if err := b.AddAllow("write(*)"); err != nil {
			t.Fatal(err)
		}
		b.Hooks = []Hook{ask}
		if got := b.Evaluate("write", true, args(map[string]string{"path": "a.txt", "content": "x"})); got.Decision != Ask || got.Step != "hook" {
			t.Fatalf("mode %v skipped a hook's ask: %+v", m, got)
		}
	}
}
