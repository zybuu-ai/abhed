package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A stored secret's value typed into a message reaches neither the model nor
// the record: both get [secret:NAME], and the user.message names what was
// withheld, once, so the person can be told.
func TestTypedSecretIsWithheldFromTheModel(t *testing.T) {
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("API_TOKEN", "tok-9f8e7d"); err != nil {
		t.Fatal(err)
	}
	if err := vault.Set("DB_PASS", "hunter2-hunter2"); err != nil {
		t.Fatal(err)
	}
	sess, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	rec := NewRecorder(store, "sess1", "")
	rec.Redact = vault.Redactor()
	ad := &scriptedAdapter{turns: []scriptedTurn{{text: "ok"}, {text: "ok"}}}
	l := NewLoop(ad, tools.NewRegistry(), policy.New(policy.ModeBypass), AutoApprove{Yes: true}, sess, rec, DefaultConfig())

	if _, err := l.Run(context.Background(), "use tok-9f8e7d and again tok-9f8e7d"); err != nil {
		t.Fatal(err)
	}
	if l.Prompt() != "use [secret:API_TOKEN] and again [secret:API_TOKEN]" {
		t.Errorf("the prompt a pipeline reads is %q", l.Prompt())
	}
	// A message sent while a run works (steer, queue) is held to the same rule.
	l.Queue("now hunter2-hunter2")
	if _, err := l.RunQueued(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ad.gotRequests) < 2 {
		t.Fatalf("%d model calls", len(ad.gotRequests))
	}
	for i, req := range ad.gotRequests {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "tok-9f8e7d") || strings.Contains(m.Content, "hunter2-hunter2") {
				t.Fatalf("call %d: the model was sent a stored value: %q", i, m.Content)
			}
		}
	}
	last := ad.gotRequests[len(ad.gotRequests)-1].Messages
	var said []string
	for _, m := range last {
		if m.Role == "user" {
			said = append(said, m.Content)
		}
	}
	if !slices.Contains(said, "use [secret:API_TOKEN] and again [secret:API_TOKEN]") || !slices.Contains(said, "now [secret:DB_PASS]") {
		t.Fatalf("the model was sent %q", said)
	}

	evs, _ := store.Events("sess1")
	var withheld []string
	for _, e := range evs {
		if e.Type != EvUserMessage {
			continue
		}
		var m Message
		_ = json.Unmarshal(e.Payload, &m)
		withheld = append(withheld, m.SecretsWithheld)
	}
	if !slices.Equal(withheld, []string{"API_TOKEN", "DB_PASS"}) {
		t.Fatalf("the user.messages name %v", withheld)
	}
}

// A message holding no stored value is sent as typed, with nothing to report.
func TestUntouchedMessageNamesNothing(t *testing.T) {
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("API_TOKEN", "tok-9f8e7d"); err != nil {
		t.Fatal(err)
	}
	sess, _ := tools.NewSession(t.TempDir())
	store := NewMemStore()
	rec := NewRecorder(store, "sess1", "")
	rec.Redact = vault.Redactor()
	ad := &scriptedAdapter{turns: []scriptedTurn{{text: "ok"}}}
	l := NewLoop(ad, tools.NewRegistry(), policy.New(policy.ModeBypass), AutoApprove{Yes: true}, sess, rec, DefaultConfig())
	if _, err := l.Run(context.Background(), "say [secret:API_TOKEN] back"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("sess1")
	for _, e := range evs {
		if e.Type == EvUserMessage && strings.Contains(string(e.Payload), "secrets_withheld") {
			t.Fatalf("a message with no value named one: %s", e.Payload)
		}
	}
	if got := ad.gotRequests[0].Messages; got[len(got)-1].Content != "say [secret:API_TOKEN] back" {
		t.Fatalf("sent %q", got[len(got)-1].Content)
	}
}

// A nil *secrets.Redactor passed as a Redactor is no redactor, as the
// recorder treats it, rather than a panic when a session starts.
func TestWithheldTextTakesATypedNilRedactor(t *testing.T) {
	var red *secrets.Redactor
	if got := WithheldText(red, "hello"); got != "hello" {
		t.Fatalf("WithheldText = %q, want the text unchanged", got)
	}
}
