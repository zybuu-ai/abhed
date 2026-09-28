package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// The agent's own write and edit calls meet a path rule by every spelling a
// disk that folds case opens, even in bypass mode.
func TestAgentFileToolsMeetRulesInAnyCase(t *testing.T) {
	dir := tempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "probe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "PROBE")); err != nil {
		t.Skip("this disk keeps case, so another spelling is another path")
	}
	secret := filepath.Join(dir, "core", "vault", "master.txt")
	if err := os.MkdirAll(filepath.Dir(secret), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, store := harnessIn(t, dir, []scriptedTurn{
		{calls: []model.ToolCall{
			call("write", map[string]string{"path": filepath.Join(dir, "core", "VAULT", "master.txt"), "content": "gone\n"}),
		}},
		{calls: []model.ToolCall{
			call("edit", map[string]string{"path": filepath.Join(dir, "Core", "Vault", "MASTER.txt"), "old_string": "keep", "new_string": "gone"}),
		}},
		{calls: []model.ToolCall{
			call("write", map[string]string{"path": filepath.Join(dir, "core", "vAuLt", "new.txt"), "content": "x"}),
		}},
		{text: "done"},
	}, policy.ModeBypass, true)
	if err := l.Policy.AddDeny("write(**/vault/**)", "edit(**/vault/**)"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(context.Background(), "change the vault"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(secret); string(got) != "keep\n" {
		t.Fatalf("the vault file was changed through another case: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "core", "vault", "new.txt")); err == nil {
		t.Fatal("a new file was written into the vault through another case")
	}
	evs, _ := store.Events("sess1")
	denied := 0
	for _, e := range evs {
		if e.Type == EvActionDenied {
			denied++
		}
	}
	if denied != 3 {
		t.Fatalf("%d calls denied, want 3", denied)
	}
}

// ManualRefused records only a denial: anything else is an error and leaves the record as it was.
func TestManualRefusedRecordsOnlyADenial(t *testing.T) {
	l, store, _ := harness(t, nil, policy.ModeDefault, true)
	args := json.RawMessage(`{"path":"/w/x"}`)
	for _, d := range []policy.Decision{policy.Allow, policy.Ask} {
		if err := l.ManualRefused("mkdir", "u1", args, policy.Result{Decision: d, Reason: "r"}); err == nil {
			t.Errorf("a %s was recorded as a refusal", d)
		}
	}
	if evs, _ := store.Events("sess1"); len(evs) != 0 {
		t.Fatalf("%d events recorded for decisions that were not denials", len(evs))
	}
	if err := l.ManualRefused("mkdir", "u2", args, policy.Result{Decision: policy.Deny, Reason: "denied by rule write(**/x)", Step: "deny"}); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("sess1")
	if len(evs) != 2 || evs[0].Type != EvActionRequested || evs[1].Type != EvActionDenied {
		t.Fatalf("a denial left %d events, want the request and its denial", len(evs))
	}
}
