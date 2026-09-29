package policy

import (
	"encoding/json"
	"testing"
)

// Every caller of Evaluate, the person's own actions included, gets arguments
// read one way only: an ambiguous one is denied, in every mode.
func TestAmbiguousArgumentsAreDeniedInEveryMode(t *testing.T) {
	for _, m := range []Mode{ModeDefault, ModeAuto, ModeBypass} {
		e := New(m)
		if err := e.AddAllow("bash(echo *)", "delete(/ws/**)", "write(/ws/**)"); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct{ tool, raw string }{
			{"bash", `{"command":"echo safe","Command":"touch /tmp/pwned"}`},
			{"bash", `{"command":"echo safe","command":"rm -rf /"}`},
			{"delete", `{"path":"/ws/a","Path":"/etc/passwd"}`},
			{"write", `{"path":"/ws/a","p\u0061th":"/etc/passwd"}`},
			{"write", `{"path":"/ws/a","\u0050ath":"/etc/passwd"}`},
			{"bash", `{"command":"echo safe"} {"command":"rm -rf /"}`},
		} {
			if d := e.Evaluate(c.tool, true, json.RawMessage(c.raw)); d.Decision != Deny || d.Step != "args" {
				t.Errorf("%s %s %s: %s at %s", m, c.tool, c.raw, d.Decision, d.Step)
			}
		}
	}
}

// A lone key in another case is read as a struct would read it.
func TestSubjectMatchesKeysAsAToolStructDoes(t *testing.T) {
	e := New(ModeBypass)
	if err := e.AddDeny("bash(rm *)", "read(/etc/**)"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ tool, raw string }{
		{"bash", `{"COMMAND":"rm -rf /"}`},
		{"read", `{"PATH":"/etc/shadow"}`},
		{"read", `{"\u0050ath":"/etc/shadow"}`},
		{"read", `{"p\u0041th":"/etc/shadow"}`},
	} {
		if d := e.Evaluate(c.tool, true, json.RawMessage(c.raw)); d.Decision != Deny || d.Step != "deny" {
			t.Errorf("%s: %s at %s", c.raw, d.Decision, d.Step)
		}
	}
}
