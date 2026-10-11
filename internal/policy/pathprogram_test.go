package policy

import "testing"

// A deny rule naming a program holds when the program is named by a path.
func TestDenyRuleMatchesAProgramNamedByItsPath(t *testing.T) {
	for _, command := range []string{
		"/usr/bin/curl x", "./curl x", "bin/curl -s x", "echo ok; /usr/bin/curl x",
		"nice -n 1 /usr/bin/curl x", "bash -c '/usr/bin/curl x'", "../../usr/bin/curl",
	} {
		e := New(ModeBypass)
		if err := e.AddDeny("bash(curl *)", "bash(curl)"); err != nil {
			t.Fatal(err)
		}
		if res := e.Evaluate("bash", true, args(map[string]string{"command": command})); res.Decision != Deny {
			t.Errorf("%q = %s at %s (%s), want deny", command, res.Decision, res.Step, res.Reason)
		}
	}
	// An allow rule still sees only the command as written.
	e := New(ModeDefault)
	if err := e.AddAllow("bash(curl *)"); err != nil {
		t.Fatal(err)
	}
	if res := e.Evaluate("bash", true, args(map[string]string{"command": "./curl x"})); res.Decision == Allow {
		t.Errorf("./curl x was allowed by bash(curl *)")
	}
}
