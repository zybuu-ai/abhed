package policy

import (
	"strings"
	"testing"
)

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

// A deny or ask rule naming flags holds with the flags moved or split.
func TestRuleFlagsMatchMovedOrSplit(t *testing.T) {
	for _, c := range []struct {
		rule, command string
		want          bool
	}{
		{"bash(rm -rf *)", "rm -r -f x", true},
		{"bash(rm -rf *)", "rm x -rf", true},
		{"bash(rm -rf *)", "rm -fr x", true},
		{"bash(rm -rf *)", "rm -v x -f -r", true},
		{"bash(rm -rf *)", "sudo /bin/rm -f x -r", true},
		{"bash(rm -rf *)", "rm -r x", false},
		{"bash(rm -rf *)", "rm -- -r -f", false},
		{"bash(git push --force *)", "git push origin --force", true},
		{"bash(git push -f *)", "git push origin main -f", true},
		{"bash(git push -f *)", "git pull -f", false},
		{"bash(rm -rf /*)", "rm / -rf", true},
		{"bash(rm -rf /*)", "rm x -rf", false},
		{"bash(chmod -R *)", "chmod 644 x -R", true},
		{"bash(chmod -R *)", "chmod 644 x", false},
		{"bash(rm -rf /*)", "rm /etc -r -f", true},
	} {
		kinds := []string{"deny"}
		if strings.HasPrefix(c.rule, "bash(chmod") {
			kinds = append(kinds, "ask") // the others confirm as destructive before any ask rule
		}
		for _, kind := range kinds {
			e := New(ModeBypass)
			add := e.AddDeny
			if kind == "ask" {
				add = e.AddAsk
			}
			if err := add(c.rule); err != nil {
				t.Fatal(err)
			}
			res := e.Evaluate("bash", true, args(map[string]string{"command": c.command}))
			if got := res.Step == kind; got != c.want {
				t.Errorf("%s %s, %q = %s at %s (%s)", kind, c.rule, c.command, res.Decision, res.Step, res.Reason)
			}
		}
	}
	// Allow rules match only as written.
	e := New(ModeDefault)
	if err := e.AddAllow("bash(ls -la *)"); err != nil {
		t.Fatal(err)
	}
	if res := e.Evaluate("bash", true, args(map[string]string{"command": "ls x -al"})); res.Decision == Allow {
		t.Error("an allow rule matched moved flags")
	}
}

// A literal brace list is not a word no rule can read, so an exec need not
// refuse it; a deny rule still sees the words it makes.
func TestLiteralBraceListIsReadNotScreened(t *testing.T) {
	e := New(ModeDefault)
	if err := e.AddDeny("bash(curl *)"); err != nil {
		t.Fatal(err)
	}
	if res := e.Evaluate("bash", true, args(map[string]string{"command": "mkdir -p a/{b,c}"})); res.Step == "screen" || res.Decision == Deny {
		t.Errorf("mkdir -p a/{b,c} = %s at %s (%s)", res.Decision, res.Step, res.Reason)
	}
	if res := e.Evaluate("bash", true, args(map[string]string{"command": "{curl,x}"})); res.Decision != Deny {
		t.Errorf("{curl,x} = %s at %s (%s), want deny", res.Decision, res.Step, res.Reason)
	}
	if res := e.Evaluate("bash", true, args(map[string]string{"command": "echo {$a,b}"})); res.Step != "screen" {
		t.Errorf("echo {$a,b} = %s at %s (%s), want a screen", res.Decision, res.Step, res.Reason)
	}
}
