package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/secrets"
)

// A stored secret's value typed into an editor's prompt never reaches the
// model: it gets [secret:NAME], and the editor is told once, in the turn.
func TestACPPromptWithholdsATypedSecret(t *testing.T) {
	r := newStudioRig(t, "")
	if err := secrets.Default().Set("API_TOKEN", "tok-9f8e7d-acp"); err != nil {
		t.Fatal(err)
	}
	id := r.open()
	_, ups := r.prompt(id, "deploy with tok-9f8e7d-acp")
	r.model.mu.Lock()
	bodies := append([]string(nil), r.model.bodies...)
	r.model.mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("the model was not called")
	}
	for _, b := range bodies {
		if strings.Contains(b, "tok-9f8e7d-acp") {
			t.Fatalf("the model was sent the value: %s", b)
		}
	}
	if !strings.Contains(bodies[0], "deploy with [secret:API_TOKEN]") {
		t.Fatalf("the model was not sent the placeholder: %s", bodies[0])
	}
	notes := 0
	for _, u := range ups {
		raw, _ := json.Marshal(u)
		if strings.Contains(string(raw), "tok-9f8e7d-acp") {
			t.Fatalf("an update carried the value: %s", raw)
		}
		if strings.Contains(string(raw), "secretsWithheld") {
			notes++
			if !strings.Contains(string(raw), "API_TOKEN") || !strings.Contains(string(raw), "not sent to the model") {
				t.Errorf("the note: %s", raw)
			}
		}
	}
	if notes != 1 {
		t.Fatalf("%d notes in %v", notes, ups)
	}
	said := false
	for _, ev := range r.events(id) {
		if ev.Type == agent.EvUserMessage {
			var m agent.Message
			_ = json.Unmarshal(ev.Payload, &m)
			said = m.SecretsWithheld == "API_TOKEN" && m.Text == "deploy with [secret:API_TOKEN]"
		}
	}
	if !said {
		t.Fatal("the record's user.message does not name what was withheld")
	}
}
