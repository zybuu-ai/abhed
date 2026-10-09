package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Deny rules written with a built-in tool name in another case, as other
// harnesses write them, hold for the agent's calls even in bypass mode.
func TestAgentMeetsDenyRulesWithToolNamesInAnyCase(t *testing.T) {
	dir := tempDir(t)
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "ran")
	l, store := harnessIn(t, dir, []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "curl -s http://example.invalid; touch " + marker})}},
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "wget http://example.invalid || touch " + marker})}},
		{calls: []model.ToolCall{call("read", map[string]string{"path": secret})}},
		{text: "done"},
	}, policy.ModeBypass, true)
	if err := l.Policy.AddDeny("Bash(curl *)", "BASH(wget*)", "Read(**/secret.txt)"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(context.Background(), "fetch it"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command a deny rule names ran")
	}
	evs, _ := store.Events("sess1")
	denied := 0
	for _, e := range evs {
		if e.Type == EvActionDenied {
			denied++
		}
		if e.Type == EvObservation && strings.Contains(string(e.Payload), "s3cret") {
			t.Fatal("the denied file was read")
		}
	}
	if denied != 3 {
		t.Fatalf("%d calls denied, want 3", denied)
	}
}

// The lines terminal and the interactive terminal's screening meet the same
// rules, and an interactive shell is screened under them.
func TestTerminalsMeetDenyRulesWithToolNamesInAnyCase(t *testing.T) {
	pol := policy.New(policy.ModeBypass)
	if err := pol.AddDeny("Bash(curl *)"); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Tools: tools.NewRegistry(tools.Bash{}), Policy: pol, Recorder: NewRecorder(NewMemStore(), "s-manual", "")}
	if _, refused, _, err := l.ManualAuthorizeTyped("u1", bashArgs("curl http://example.invalid"), Confirmed); err != nil || refused == nil {
		t.Fatalf("lines terminal: refused %v err %v", refused, err)
	}
	if refused, err := l.ManualScreen("u2", "curl http://example.invalid"); err != nil || refused == nil {
		t.Fatalf("interactive terminal: refused %v err %v", refused, err)
	}
	if !pol.Screens("bash") {
		t.Fatal("an interactive shell would run unscreened under a Bash(...) deny rule")
	}
}
