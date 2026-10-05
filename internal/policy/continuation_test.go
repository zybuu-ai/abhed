package policy

import (
	"strings"
	"testing"
)

// A line continuation or a word-splitting expansion must not hide a command
// from the deny, destructive and ask steps, in any mode.
func TestContinuationsAndSplittingDoNotHideCommands(t *testing.T) {
	cases := []struct {
		command string
		want    Decision
		step    string
	}{
		{"rm \\\n-rf scratch", Ask, "destructive"},
		{"rm -\\\nrf scratch", Ask, "destructive"},
		{"rm \\\n-r scratch", Ask, "destructive"},
		{"rm scratch \\\n-rf", Ask, "destructive"},
		{"git reset \\\n--hard HEAD~1", Ask, "destructive"},
		{"git push \\\n--force origin main", Ask, "destructive"},
		{"rm${IFS}-rf${IFS}scratch", Ask, "destructive"},
		{"rm\t-rf scratch", Ask, "destructive"},
		{"rm$'\\x20'-rf$'\\x20'scratch", Ask, "destructive"},
		{"{rm,-rf,scratch}", Ask, "destructive"},
		// A changed IFS hides words from the deny rule, which must still hold.
		{"IFS=,; x=rm,-rf,scratch; $x", Deny, "screen"},
		{"git \\\nstash", Ask, "ask"},
		{"git${IFS}stash", Ask, "ask"},
		{"{git,stash}", Ask, "ask"},
		{"cu\\\nrl x", Deny, "deny"},
		{"curl${IFS}x", Deny, "deny"},
		{"cu\\\nrl\tx", Deny, "deny"},
		// An expansion glues words no rule can read, so a rule-bearing policy asks.
		{"echo${IFS}hi", Ask, "screen"},
	}
	for _, mode := range []Mode{ModeBypass, ModeAuto} {
		for _, c := range cases {
			e := New(mode)
			if err := e.AddDeny("bash(curl *)"); err != nil {
				t.Fatal(err)
			}
			if err := e.AddAsk("bash(git stash*)"); err != nil {
				t.Fatal(err)
			}
			res := e.Evaluate("bash", true, args(map[string]string{"command": c.command}))
			if res.Decision != c.want || res.Step != c.step {
				t.Errorf("%s %q = %s at %s (%s), want %s at %s", mode, c.command, res.Decision, res.Step, res.Reason, c.want, c.step)
			}
		}
	}
}

// With no rules, bypass still asks for a hidden destructive command, and a
// continuation is named in the reason the approver reads.
func TestContinuationIsNamedInTheReason(t *testing.T) {
	res := New(ModeBypass).Evaluate("bash", true, args(map[string]string{"command": "rm \\\n-rf scratch"}))
	if res.Decision != Ask || res.Step != "destructive" || !strings.Contains(res.Reason, "backslash-newline") {
		t.Fatalf("got %s at %s (%s)", res.Decision, res.Step, res.Reason)
	}
	res = New(ModeAuto).Evaluate("bash", true, args(map[string]string{"command": "rm scratch \\\n-rf"}))
	if res.Step != "destructive" {
		t.Fatalf("auto: got %s at %s (%s)", res.Decision, res.Step, res.Reason)
	}
}

// A legitimate multi-line command is judged on its joined form: an allow rule
// that fits it joined approves it, and a tricked one never widens an allow.
func TestContinuationJoinedFormAndNoWidening(t *testing.T) {
	e := New(ModeDefault)
	if err := e.AddAllow("bash(go test *)"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAsk("bash(git stash*)"); err != nil {
		t.Fatal(err)
	}
	for command, want := range map[string]Decision{
		"go test \\\n  ./...":         Allow,
		"go test ./...":               Allow,
		"go test \\\n ./... ; rm x":   Ask,
		"go test\t./...":              Ask,
		"go${IFS}test ./...":          Ask,
		"{go,test} ./...":             Ask,
		"go test $'\\x20'./...":       Ask,
		"go test \\\n  ./... \\\n -v": Allow,
	} {
		res := e.Evaluate("bash", true, args(map[string]string{"command": command}))
		if res.Decision != want {
			t.Errorf("%q = %s at %s (%s), want %s", command, res.Decision, res.Step, res.Reason, want)
		}
	}
	// With no rule to hide from, a joined read-only command runs in bypass.
	if res := New(ModeBypass).Evaluate("bash", true, args(map[string]string{"command": "ls \\\n -la"})); res.Decision != Allow {
		t.Fatalf("bypass ls continuation: %s at %s (%s)", res.Decision, res.Step, res.Reason)
	}
}
