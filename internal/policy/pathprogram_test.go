package policy

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/managed"
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
		// The star glued to the flags, as an org file writes it.
		{"bash(rm -rf*)", "rm -r -f build", true},
		{"bash(rm -rf*)", "rm -fr build", true},
		{"bash(rm -rf*)", "rm build -f -r", true},
		{"bash(rm -rf*)", "rm -rfv build", true},
		{"bash(rm -rf*)", "rm -r build", false},
		{"bash(rm -rf*)", "rmdir -r -f build", false},
		{"bash(chmod -R*)", "chmod 644 x -R", true},
		{"bash(git push -f*)", "git push origin main -f", true},
		{"bash(git push -f*)", "git pull -f", false},
		// Flags that do the same as the rule's (architect review of the eval fixes).
		{"bash(rm -rf*)", "rm -Rf build", true},
		{"bash(rm -rf*)", "rm -R -f build", true},
		{"bash(rm -rf*)", "rm --recursive --force build", true},
		{"bash(rm -rf *)", "rm --recursive -f build", true},
		{"bash(rm -rf *)", "rm -R --force build", true},
		{"bash(rm --recursive --force *)", "rm -rf build", true},
		{"bash(rm -rf*)", "rm --recursive build", false},
		{"bash(chmod -R*)", "chmod --recursive 777 /srv", true},
		{"bash(chmod -R*)", "chmod -r x", false},
		{"bash(chown -R *)", "chown --recursive me /srv", true},
		{"bash(git push -f*)", "git push --force origin main", true},
		{"bash(git push -f*)", "git push --force-with-lease origin main", true},
		{"bash(git push -f*)", "git push --force-with-lease=main origin main", true},
		{"bash(git push --force *)", "git push -f origin", true},
		{"bash(git push -f*)", "git -C /repo push -f origin", true},
		{"bash(git push -f*)", "git -c core.x=y --no-pager push --force origin", true},
		{"bash(git push -f*)", "git --git-dir=/r/.git --work-tree /r push -f", true},
		{"bash(git push -f*)", "git -C /repo pull -f", false},
		{"bash(git clean -fdx*)", "git clean --force -d -x", true},
		{"bash(git clean -fdx*)", "git -C /r clean -x -d -f", true},
		{"bash(git clean -fdx*)", "git clean -f -d", false},
		// Architect review r1: long flags with a glued star.
		{"bash(git push --force*)", "git push -f", true},
		{"bash(git push --force*)", "git push -f origin", true},
		{"bash(git push --force*)", "git -C d push --force origin", true},
		{"bash(rm --recursive*)", "rm -r x", true},
		{"bash(rm --recursive*)", "rm -rf x", true},
		{"bash(chmod --recursive*)", "chmod -R 644 x", true},
		{"bash(git push --force*)", "git push origin", false},
		// Every git option before the subcommand is skipped.
		{"bash(git push -f*)", "git --exec-path=/tmp/x push -f", true},
		{"bash(git push -f*)", "git --config-env=a.b=ENV push -f", true},
		{"bash(git push -f*)", "git --config-env a.b=ENV push -f", true},
		{"bash(git push -f*)", "git --no-optional-locks push -f", true},
		{"bash(git push -f*)", "git --glob-pathspecs --no-advice push -f", true},
		{"bash(git push -f*)", "git --attr-source=HEAD --super-prefix=x/ push -f", true},
		{"bash(git push -f*)", "git -Cdir push -f", true},
		{"bash(git push -f*)", "git -cx=y push -f", true},
		{"bash(git push -f*)", "git -C dir -C d2 push -f", true},
		{"bash(git push -f*)", "git --git-dir .git push --force-with-lease=main", true},
		{"bash(git push -f*)", "git -c x=y log -f", false},
		{"bash(git push -f*)", "git pull --force", false},
		// A refspec with + forces as -f does.
		{"bash(git push -f*)", "git push origin +main", true},
		{"bash(git push -f *)", "git -C d push origin +main:main", true},
		{"bash(git push -f*)", "git push origin main", false},
		// GNU abbreviations of long options (eval r2 #12).
		{"bash(rm -rf*)", "rm --recur --forc build", true},
		{"bash(rm -rf *)", "rm --recursiv --force build", true},
		{"bash(chmod -R*)", "chmod --recursiv 644 /srv", true},
		{"bash(git clean -f*)", "git clean --forc", true},
		{"bash(git push -f*)", "git push --forc origin", true},
		{"bash(git push -f*)", "git push --force-with origin", true},
		{"bash(rm -rf*)", "rm --re --fo build", false},
		{"bash(rm --no-preserve-root *)", "rm --no-pres /", true},
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
	// Allow rules match only as written: a starred cluster's, and no synonym
	// or git global option widens one.
	for rule, cmds := range map[string][]string{
		"bash(rm -rf*)":           {"rm -r -f build", "rm -fr build", "rm build -rf", "rm -Rf build", "rm --recursive --force build"},
		"bash(git push -f*)":      {"git push --force origin", "git -C /r push -f origin", "git push --force-with-lease origin", "git push origin +main", "git --no-optional-locks push -f"},
		"bash(git push --force*)": {"git push -f", "git push --force origin"},
		"bash(rm --recursive*)":   {"rm -r x", "rm --recursive x"},
		"bash(rm -rf *)":          {"rm --recur --forc build", "rm --recursiv -f build"},
		"bash(git clean -f *)":    {"git clean --forc"},
		"bash(chmod -R*)":         {"chmod --recursive 777 /srv"},
		"bash(git clean -f*)":     {"git clean --force", "git -C /r clean -f"},
	} {
		for _, cmd := range cmds {
			e := New(ModeDefault)
			if err := e.AddAllow(rule); err != nil {
				t.Fatal(err)
			}
			if res := e.Evaluate("bash", true, args(map[string]string{"command": cmd})); res.Step == "allow" {
				t.Errorf("allow %s let %q through", rule, cmd)
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
	// Readable loops and sequences run unasked in bypass under a deny rule,
	// and a deny rule sees the words each bash version makes of them.
	by := New(ModeBypass)
	if err := by.AddDeny("bash(curl *)", "bash(kill -9 *)", "bash(git reset --hard*)"); err != nil {
		t.Fatal(err)
	}
	for command, want := range map[string]Decision{
		"for i in {1..10}; do echo $i; done":   Allow,
		"for i in {1..2000}; do echo $i; done": Allow,
		"touch f{01..10}.txt":                  Allow,
		"echo {1..10..2} {Z..a}":               Allow,
		"kill -{09..09} 1":                     Deny,
		"git reset --{h..h..1}ard":             Deny,
		"bash -c 'kill -{9,} 1'":               Deny,
		"echo {1..20000}":                      Ask,
	} {
		if res := by.Evaluate("bash", true, args(map[string]string{"command": command})); res.Decision != want {
			t.Errorf("bypass %q = %s at %s (%s), want %s", command, res.Decision, res.Step, res.Reason, want)
		}
	}
	// A list that makes the program from an expansion hides it.
	if res := e.Evaluate("bash", true, args(map[string]string{"command": "{$a,b} x"})); res.Decision != Ask || res.Step == "mode" {
		t.Errorf("{$a,b} x = %s at %s (%s), want an ask before the mode", res.Decision, res.Step, res.Reason)
	}
}

// Naming workspace trust asks in every mode, with no ask rule configured.
func TestTrustNamingAsksWithoutAnAskRule(t *testing.T) {
	for _, command := range []string{
		"ABHED_TRUST_WORKSPACE=1 abhed -p hi", "abhed --Trust-Workspace -p hi",
		"X=abhed_trust_workspace; echo $X", "bash -c 'abhed -trust-workspace -p hi'",
	} {
		for _, mode := range []Mode{ModeBypass, ModeAuto, ModeDefault} {
			e := New(mode)
			if err := e.AddAllow("bash(*)"); err != nil {
				t.Fatal(err)
			}
			res := e.Evaluate("bash", true, args(map[string]string{"command": command}))
			if res.Decision != Ask || !strings.Contains(res.Reason, "nested run") {
				t.Errorf("%s %q = %s at %s (%s), want the trust ask", mode, command, res.Decision, res.Step, res.Reason)
			}
		}
	}
}

// Bypass under a managed file asks, and the reason names the file.
func TestManagedBypassReasonNamesTheFile(t *testing.T) {
	e := New(ModeBypass)
	e.Managed = true
	res := e.Evaluate("bash", true, args(map[string]string{"command": "make"}))
	if res.Decision != Ask || !strings.Contains(res.Reason, managed.ConfigFile) {
		t.Errorf("= %s (%s), want an ask naming %s", res.Decision, res.Reason, managed.ConfigFile)
	}
}
