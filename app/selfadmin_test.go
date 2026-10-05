package app

import (
	"os"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

func asAgentCommand(t *testing.T) {
	t.Helper()
	old := inAgentCommand
	inAgentCommand = func() string { return "ABHED_SANDBOX is set" }
	t.Cleanup(func() { inAgentCommand = old })
}

// An agent's own bash cannot administer Abhed: prune its record, grant trust,
// change accounts, secrets or MCP servers, or start a session with more power.
func TestSelfAdministrationRefusedInAgentCommand(t *testing.T) {
	ws := t.TempDir()
	ids := recordHome(t, ws, "keep this")
	// A model no session can reach, should a refusal fail and a session start.
	home := os.Getenv("HOME") + "/.abhed"
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// The person's own deny rule, which a nested session's settings must keep.
	unreachable := `{"model":{"default":"none","providers":{"none":{"type":"openai-compatible","base_url":"http://127.0.0.1:1/v1","model":"x"}}},"permissions":{"deny":["bash(curl*)"]}}`
	if err := os.WriteFile(home+"/config.json", []byte(unreachable), 0o600); err != nil {
		t.Fatal(err)
	}
	settingsFile := t.TempDir() + "/nested.json"
	if err := os.WriteFile(settingsFile, []byte(`{"permissions":{"allow":["bash(*)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	asAgentCommand(t)
	for _, args := range [][]string{
		{"record", "prune", ids[0], "-yes"},
		{"record", "prune", "-older-than", "0d", "-yes"},
		{"trust", "grant"},
		{"init"},
		{"user", "add", "eve"},
		{"user", "passwd", "eve"},
		{"secret", "set", "X"},
		{"secret", "rm", "X"},
		{"mcp", "add", "x", "/bin/true"},
		{"mcp", "remove", "x"},
		{"migrate"},
		{"serve", "-trust-workspace"},
		{"-trust-workspace", "-p", "hi"},
		{"-dangerously-skip-permissions", "-p", "hi"},
		{"-mode", "bypass", "-p", "hi"},
		// The merged configuration counts, not only the flags that name a mode.
		{"-settings", `{"permissions":{"mode":"bypass"}}`, "-p", "hi"},
		{"-settings", settingsFile, "-p", "hi"},
		{"-settings", `{"permissions":{"git_extensions":["lfs"]}}`, "-p", "hi"},
		{"-allowedTools", "Bash(curl:*)", "-p", "hi"},
		// Settings replace lists, so an empty one would drop the person's rules.
		{"-settings", `{"permissions":{"deny":[]}}`, "-p", "hi"},
		{"-settings", `{"permissions":{"ask":[]}}`, "-p", "hi"},
	} {
		out, code := stderrOf(t, append([]string{"-C", ws}, args...))
		if code != 1 || !strings.Contains(out, "refused inside an agent's command") {
			t.Errorf("%v: exit %d, %q", args, code, out)
		}
	}
	if code, out, _ := runRecord(t, ws, "list"); code != 0 || !strings.Contains(out, ids[0]) {
		t.Fatalf("the session was pruned: %d %s", code, out)
	}
	// Reading, and a session with one more deny rule, are still allowed.
	for _, args := range [][]string{{"record", "list"}, {"trust", "show"}, {"mcp", "list"},
		{"-settings", `{"permissions":{"deny":["bash(curl*)","bash(wget*)"]}}`, "-p", "hi"},
		{"-disallowedTools", "Bash(wget:*)", "-p", "hi"}} {
		if out, _ := stderrOf(t, append([]string{"-C", ws}, args...)); strings.Contains(out, "refused inside") {
			t.Errorf("%v refused: %s", args, out)
		}
	}
}

func TestSelfAdminNamesOnlyChanges(t *testing.T) {
	for args, want := range map[string]bool{
		"record prune x": true, "record list": false, "record verify": false,
		"trust grant": true, "trust show": false, "trust revoke": false,
		"user add a": true, "user list": false, "secret set A": true, "secret list": false,
		"mcp add a b": true, "mcp rm a": true, "mcp remove a": true, "mcp list": false, "init": true, "doctor": false,
		"eval -trust-workspace": true, "resolve --trust-workspace=true u": true,
	} {
		if got := selfAdmin(strings.Fields(args)) != ""; got != want {
			t.Errorf("selfAdmin(%q) = %v, want %v", args, got, want)
		}
	}
}

// A rule or mode the configuration already holds is not a widening.
func TestWidenedNamesOnlyAdditions(t *testing.T) {
	var base config.Config
	base.Permissions.Allow = []string{"read"}
	eff := base
	eff.Permissions.Deny = []string{"bash(rm*)"}
	if got := widened(base, eff); got != "" {
		t.Errorf("a deny rule: %q", got)
	}
	base.Permissions.Mode, eff.Permissions.Mode = "bypass", "bypass"
	if got := widened(base, eff); got != "" {
		t.Errorf("bypass already configured: %q", got)
	}
}

// Dropping a deny or ask rule the configuration holds widens a session.
func TestWidenedNamesRemovedRules(t *testing.T) {
	var base config.Config
	base.Permissions.Deny = []string{"bash(curl*)"}
	base.Permissions.Ask = []string{"bash(*trust-workspace*)"}
	eff := base
	eff.Permissions.Deny = nil
	if got := widened(base, eff); !strings.Contains(got, "deny rule removed") {
		t.Errorf("deny removed: %q", got)
	}
	eff = base
	eff.Permissions.Ask = []string{}
	if got := widened(base, eff); !strings.Contains(got, "ask rule removed") {
		t.Errorf("ask removed: %q", got)
	}
}
