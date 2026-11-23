package app

import (
	"encoding/json"
	"strconv"
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

// A pasted `oc login` reaches neither the model nor any update; the note
// names it and carries the message as recorded, with its seq, for the
// editor to redraw the bubble it drew as typed.
func TestACPPromptWithholdsAPastedLogin(t *testing.T) {
	const tok = "sha256~Zq1cP3xYv8mN0aB4dE6fG7hI9jK2lM5nO8pQrStUvWx"
	r := newStudioRig(t, "")
	id := r.open()
	_, ups := r.prompt(id, "oc login --token="+tok+" --server=https://api.lab.example:6443")
	r.model.mu.Lock()
	bodies := append([]string(nil), r.model.bodies...)
	r.model.mu.Unlock()
	for _, b := range bodies {
		if strings.Contains(b, tok) {
			t.Fatalf("the model was sent the token")
		}
	}
	var seq int64
	for _, ev := range r.events(id) {
		if ev.Type == agent.EvUserMessage {
			seq = ev.Seq
		}
	}
	notes := 0
	for _, u := range ups {
		raw, _ := json.Marshal(u)
		if strings.Contains(string(raw), tok) {
			t.Fatalf("an update carried the token: %s", raw)
		}
		if strings.Contains(string(raw), "pastedCredentials") {
			notes++
			want := `"text":"oc login --token=[pasted:P1] --server=https://api.lab.example:6443"`
			if !strings.Contains(string(raw), want) || !strings.Contains(string(raw), `"seq":`+strconv.FormatInt(seq, 10)) ||
				!strings.Contains(string(raw), `"id":"P1"`) {
				t.Errorf("the note: %s (seq %d)", raw, seq)
			}
		}
	}
	if notes != 1 {
		t.Fatalf("%d notes", notes)
	}
}
