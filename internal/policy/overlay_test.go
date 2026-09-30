package policy

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func cmd(c string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": c})
	return b
}

func pathArg(p string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"path": p})
	return b
}

// A session allow is evaluated with the allow rules, so it can never lift a
// deny rule, a destructive command, an ask rule or plan mode; in every mode
// where those would stop a call, they still do.
func TestSessionAllowCannotLiftDenyDestructiveAskOrPlan(t *testing.T) {
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits, ModeAuto, ModePlan, ModeBypass} {
		e := New(mode)
		e.Session = &Overlay{}
		if err := e.AddDeny("bash(curl *)"); err != nil {
			t.Fatal(err)
		}
		if err := e.AddAsk("bash(git push*)"); err != nil {
			t.Fatal(err)
		}
		for _, r := range []string{"bash(curl *)", "bash(rm -rf *)", "bash(git push*)", "write(**)", "bash(*)"} {
			if _, err := e.Session.Add(ListAllow, r); err != nil {
				t.Fatal(err)
			}
		}
		if got := e.Evaluate("bash", true, cmd("curl http://x")); got.Decision != Deny || got.Step != "deny" {
			t.Errorf("%s: a session allow lifted a deny rule: %+v", mode, got)
		}
		if got := e.Evaluate("bash", true, cmd("rm -rf build")); got.Decision == Allow || mode != ModePlan && got.Step != "destructive" {
			t.Errorf("%s: a session allow approved a destructive command: %+v", mode, got)
		}
		if mode != ModePlan {
			if got := e.Evaluate("bash", true, cmd("git push origin")); got.Decision != Ask || got.Step != "ask" {
				t.Errorf("%s: a session allow beat an ask rule: %+v", mode, got)
			}
		}
		if mode == ModePlan {
			if got := e.Evaluate("write", true, pathArg("/w/a.go")); got.Decision != Deny || got.Step != "mode" {
				t.Errorf("plan: a session allow approved a write: %+v", got)
			}
		}
	}
}

// A session allow approves what would otherwise ask, and says it was the
// session's rule; the configured rules are named as rules.
func TestSessionAllowApprovesWhatWouldAskAndIsNamed(t *testing.T) {
	e := New(ModeDefault)
	e.Session = &Overlay{}
	if got := e.Evaluate("bash", true, cmd("go test ./...")); got.Decision != Ask || got.Rule != "" {
		t.Fatalf("before: %+v", got)
	}
	if _, err := e.Session.Add(ListAllow, "bash(go test*)"); err != nil {
		t.Fatal(err)
	}
	got := e.Evaluate("bash", true, cmd("go test ./..."))
	if got.Decision != Allow || got.Step != "allow" || got.Rule != "bash(go test*)" || !strings.Contains(got.Reason, "session rule bash(go test*)") {
		t.Fatalf("after: %+v", got)
	}
	if got := e.Evaluate("bash", true, cmd("go vet ./...")); got.Decision != Ask {
		t.Fatalf("a narrow session rule approved another command: %+v", got)
	}
}

// Session deny and ask rules tighten over a configured allow.
func TestSessionDenyAndAskTighten(t *testing.T) {
	e := New(ModeAuto)
	e.Session = &Overlay{}
	if err := e.AddAllow("bash(go *)"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Session.Add(ListDeny, "bash(go generate*)"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Session.Add(ListAsk, "edit(secrets/**)"); err != nil {
		t.Fatal(err)
	}
	if got := e.Evaluate("bash", true, cmd("go generate ./...")); got.Decision != Deny || got.Rule != "bash(go generate*)" {
		t.Fatalf("session deny: %+v", got)
	}
	if got := e.Evaluate("edit", true, pathArg("secrets/a")); got.Decision != Ask || got.Step != "ask" {
		t.Fatalf("session ask in auto mode: %+v", got)
	}
	if !e.Screens("bash") {
		t.Fatal("a session deny rule on bash is not reported as screening it")
	}
}

// Clear ends the session's rules and keeps the pinned ones; adding a rule
// twice keeps one, and removing names the list.
func TestOverlayClearKeepsPinned(t *testing.T) {
	e := New(ModeAcceptEdits)
	e.Session = &Overlay{}
	if added, _ := e.Session.Add(ListAllow, "bash(make)"); !added {
		t.Fatal("not added")
	}
	if added, _ := e.Session.Add(ListAllow, " bash(make) "); added {
		t.Fatal("added twice")
	}
	if err := e.Session.Pin("read-only /x", "write(/x/**)", "edit(/x/**)"); err != nil {
		t.Fatal(err)
	}
	e.Session.Clear()
	if got := e.Evaluate("bash", true, cmd("make")); got.Decision != Ask {
		t.Fatalf("a cleared session rule still allowed: %+v", got)
	}
	if got := e.Evaluate("write", true, pathArg("/x/a")); got.Decision != Deny {
		t.Fatalf("a pinned rule did not survive Clear: %+v", got)
	}
	if _, err := e.Session.Add("maybe", "bash"); err == nil {
		t.Fatal("an unknown list was accepted")
	}
	if removed, _ := e.Session.Remove(ListAllow, "bash(make)"); removed {
		t.Fatal("removed a rule that was cleared")
	}
}

// Each step that a rule decides names the rule.
func TestResultNamesTheRule(t *testing.T) {
	e := New(ModeDefault)
	_ = e.AddDeny("bash(sudo *)")
	_ = e.AddAsk("bash(git *)")
	_ = e.AddAllow("bash(ls*)")
	for _, c := range []struct{ command, step, rule string }{
		{"sudo x", "deny", "bash(sudo *)"},
		{"git status", "ask", "bash(git *)"},
		{"ls -l", "allow", "bash(ls*)"},
		{"make", "default", ""},
	} {
		if got := e.Evaluate("bash", true, cmd(c.command)); got.Step != c.step || got.Rule != c.rule {
			t.Errorf("%s: %+v", c.command, got)
		}
	}
}

func TestIsBroad(t *testing.T) {
	for rule, want := range map[string]bool{
		"bash": true, "bash(*)": true, "write(**)": true, "edit(**/*)": true, "*": true, "read": true,
		"bash(go test*)": false, "write(src/**)": false, "web_fetch(https://go.dev/*)": false,
	} {
		if IsBroad(rule) != want {
			t.Errorf("IsBroad(%s) = %v", rule, !want)
		}
	}
}

// The session's rules change while calls are evaluated on other goroutines.
func TestOverlayIsSafeUnderConcurrentEvaluation(t *testing.T) {
	e := New(ModeDefault)
	e.Session = &Overlay{}
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				e.Evaluate("bash", true, cmd("go test"))
				_ = e.Screens("bash")
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				_, _ = e.Session.Add(ListAllow, "bash(go test*)")
				_, _ = e.Session.Add(ListDeny, "bash(x"+string(rune('a'+i))+")")
				if j%10 == 0 {
					e.Session.Clear()
				}
			}
		}()
	}
	wg.Wait()
}

// A hook's allow is no opinion: it approves nothing that would ask and lifts
// nothing that is denied. Its ask waits for the deny rules and plan mode.
func TestHookAllowIsNoOpinionAndAskWaitsForDeny(t *testing.T) {
	allow := func(string, json.RawMessage) *Result { return &Result{Decision: Allow, Reason: "a hook says yes"} }
	ask := func(string, json.RawMessage) *Result { return &Result{Decision: Ask, Reason: "a hook asks"} }
	e := New(ModeDefault)
	if err := e.AddDeny("bash(curl *)"); err != nil {
		t.Fatal(err)
	}
	e.Hooks = []Hook{allow}
	if got := e.Evaluate("bash", true, cmd("curl x")); got.Decision != Deny {
		t.Fatalf("a hook's allow lifted a deny: %+v", got)
	}
	if got := e.Evaluate("bash", true, cmd("make")); got.Decision != Ask {
		t.Fatalf("a hook's allow approved a call that asks: %+v", got)
	}
	e.Hooks = []Hook{allow, ask}
	if got := e.Evaluate("bash", true, cmd("curl x")); got.Decision != Deny {
		t.Fatalf("a hook's ask turned a deny into a question: %+v", got)
	}
	if got := e.Evaluate("bash", true, cmd("ls")); got.Decision != Ask || got.Step != "hook" {
		t.Fatalf("a hook's ask was lost behind an allow: %+v", got)
	}
}
