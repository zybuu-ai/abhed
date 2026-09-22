package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

func secretHarness(t *testing.T, allow bool) (*Loop, *MemStore) {
	t.Helper()
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("API_TOKEN", "tok-9f8e7d"); err != nil {
		t.Fatal(err)
	}
	sess, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	rec := NewRecorder(store, "sess1", "")
	rec.Redact = vault.Redactor()
	reg := tools.NewRegistry(tools.Bash{Secrets: vault.Env, SecretNames: []string{"API_TOKEN"}})
	pol := policy.New(policy.ModeBypass)
	if allow {
		_ = pol.AddAllow("secret(API_TOKEN)")
	}
	turns := []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]any{"command": "echo token=$API_TOKEN", "description": "use it", "secrets": []string{"API_TOKEN"}})}},
		{text: "done"},
	}
	return NewLoop(&scriptedAdapter{turns: turns}, reg, pol, AutoApprove{Yes: true}, sess, rec, DefaultConfig()), store
}

// Without an allow rule for the name, even bypass mode refuses the command.
func TestSecretNeedsItsOwnAllowRule(t *testing.T) {
	l, store := secretHarness(t, false)
	l.Run(context.Background(), "go")
	evs, _ := store.Events("sess1")
	if !hasEvent(evs, EvActionDenied) {
		t.Fatal("a command asking for a secret with no allow rule was not denied")
	}
	for _, e := range evs {
		if strings.Contains(string(e.Payload), "tok-9f8e7d") {
			t.Fatal("the value reached the record")
		}
	}
	last := l.Adapter.(*scriptedAdapter).gotRequests[1]
	told := false
	for _, m := range last.Messages {
		told = told || strings.Contains(m.Content, "secret(API_TOKEN)")
	}
	if !told {
		t.Fatal("the model was not told which rule would permit the secret")
	}
}

// With the rule, the command sees the value and the record sees the name.
func TestSecretReachesTheCommandButNotTheRecord(t *testing.T) {
	l, store := secretHarness(t, true)
	l.Run(context.Background(), "go")
	evs, _ := store.Events("sess1")
	redacted := false
	for _, e := range evs {
		if strings.Contains(string(e.Payload), "tok-9f8e7d") {
			t.Fatalf("the value reached the record: %s", e.Payload)
		}
		if e.Type == EvObservation && strings.Contains(string(e.Payload), "token=[secret:API_TOKEN]") {
			redacted = true
		}
	}
	if !redacted {
		t.Fatal("the command did not receive the secret, or the record was not redacted")
	}
	last := l.Adapter.(*scriptedAdapter).gotRequests[1]
	for _, m := range last.Messages {
		if strings.Contains(m.Content, "tok-9f8e7d") {
			t.Fatal("the model saw the value")
		}
	}
}
