package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

type hostSubject struct{}

func (hostSubject) PolicySubject(args json.RawMessage) (string, bool) {
	var a struct{ Host, Command string }
	if json.Unmarshal(args, &a) != nil || a.Host == "" {
		return "", false
	}
	return a.Host + "/" + a.Command, true
}

// A tool that names its own subject is judged on it: an allow rule or scope
// for one host never covers another, and deny rules on the command still hold.
func TestSubjectorNamesTheSubject(t *testing.T) {
	tools.RegisterSubjector("test_remote", hostSubject{})
	e := New(ModeDefault)
	if err := e.AddAllow("test_remote(staging/*)"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddDeny("test_remote(rm *)"); err != nil {
		t.Fatal(err)
	}
	call := func(host, command string) Result {
		return e.Evaluate("test_remote", true, args(map[string]string{"host": host, "command": command}))
	}
	if res := call("staging", "uptime"); res.Decision != Allow {
		t.Errorf("staging uptime = %s (%s)", res.Decision, res.Reason)
	}
	if res := call("prod", "uptime"); res.Decision != Ask || res.Offer() != "test_remote(prod/uptime)" {
		t.Errorf("prod uptime = %s offer %q (%s)", res.Decision, res.Offer(), res.Reason)
	}
	if res := call("staging", "rm -rf /"); res.Decision != Deny {
		t.Errorf("staging rm = %s (%s)", res.Decision, res.Reason)
	}
	if got := Subject("test_remote", args(map[string]string{"host": "prod", "command": "id"})); got != "prod/id" {
		t.Errorf("Subject = %q", got)
	}
}

// The first subject argument decides: a list of strings is read as words, and
// any other kind is refused rather than passed over for the next argument.
func TestNonStringSubjectIsNotPassedOver(t *testing.T) {
	e := New(ModeBypass)
	if err := e.AddDeny("mcp__x__run(curl *)"); err != nil {
		t.Fatal(err)
	}
	if res := e.Evaluate("mcp__x__run", true, json.RawMessage(`{"command":["curl","x"],"path":"a"}`)); res.Decision != Deny || res.Step != "deny" {
		t.Errorf("list command = %s at %s (%s)", res.Decision, res.Step, res.Reason)
	}
	for _, raw := range []string{`{"command":{"argv":"curl x"},"path":"a"}`, `{"command":7,"path":"a"}`, `{"command":["curl",1],"path":"a"}`} {
		res := e.Evaluate("mcp__x__run", true, json.RawMessage(raw))
		if res.Decision != Deny || res.Step != "args" || !strings.Contains(res.Reason, "must be a string") {
			t.Errorf("%s = %s at %s (%s)", raw, res.Decision, res.Step, res.Reason)
		}
	}
	if res := e.Evaluate("mcp__x__run", true, json.RawMessage(`{"command":null,"path":"a"}`)); res.Step == "args" {
		t.Errorf("a null subject was refused: %s", res.Reason)
	}
}
