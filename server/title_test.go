package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

func listed(t *testing.T, s *Server, user, tenant string) []sessionSummary {
	t.Helper()
	var list []sessionSummary
	if err := json.Unmarshal(callAs(t, s, user, tenant, "GET", "/v1/sessions", "").Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func renames(events []agent.Event) []agent.SessionRenamed {
	var out []agent.SessionRenamed
	for _, ev := range events {
		if ev.Type == agent.EvSessionRenamed {
			var rn agent.SessionRenamed
			_ = json.Unmarshal(ev.Payload, &rn)
			if ev.Actor != agent.ActorUser {
				rn.By = "not the user: " + string(ev.Actor)
			}
			out = append(out, rn)
		}
	}
	return out
}

// A rename is recorded with who made it and the title it replaced, the list
// shows it, the opening prompt is kept, and it survives a restart.
func TestRenameIsRecordedAndListed(t *testing.T) {
	s, st, _, _ := switchServer(t, func(c *config.Config) { c.Auth.Mode = "proxy" })
	id := sessionOf(t, callAs(t, s, "ana", "", "POST", "/v1/sessions", `{"prompt":"fix the retry loop"}`))
	turnsEnded(t, st, id, 1)

	if w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+id+"/title", `{"title":"  Retry fix  "}`); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"title":"Retry fix"`) {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	if w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+id+"/title", `{"title":"Retry, take two"}`); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"from":"Retry fix"`) {
		t.Fatalf("second rename: %d %s", w.Code, w.Body.String())
	}
	events, _ := st.Events(id)
	got := renames(events)
	if len(got) != 2 || got[0].Title != "Retry fix" || got[0].By != "ana" || got[1].From != "Retry fix" || got[1].Title != "Retry, take two" {
		t.Fatalf("the record holds %+v", got)
	}
	list := listed(t, s, "ana", "")
	if len(list) != 1 || list[0].Title != "Retry, take two" || list[0].Prompt != "fix the retry loop" {
		t.Fatalf("the list shows %+v", list)
	}

	// After a restart the title comes from the row, and a reopened session from its record.
	forget(s, id)
	if list := listed(t, s, "ana", ""); len(list) != 1 || list[0].Title != "Retry, take two" {
		t.Fatalf("after a restart the list shows %+v", list)
	}
	if w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+id+"/title", `{"title":""}`); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"from":"Retry, take two"`) {
		t.Fatalf("clearing the title after a restart: %d %s", w.Code, w.Body.String())
	}
	if list := listed(t, s, "ana", ""); len(list) != 1 || list[0].Title != "" {
		t.Fatalf("a cleared title still shows: %+v", list)
	}
}

// Only the owner renames, a title with a line break or past the limit is
// refused, and a session mid-turn is not renamed. Nothing refused is recorded.
func TestRenameRefusals(t *testing.T) {
	s, st, a, _ := switchServer(t, func(c *config.Config) { c.Auth.Mode = "proxy" })
	id := sessionOf(t, callAs(t, s, "ana", "", "POST", "/v1/sessions", `{"prompt":"one"}`))
	turnsEnded(t, st, id, 1)

	if w := callAs(t, s, "bo", "", "POST", "/v1/sessions/"+id+"/title", `{"title":"mine now"}`); w.Code != http.StatusNotFound {
		t.Fatalf("another user's rename: %d %s", w.Code, w.Body.String())
	}
	if w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+id+"/title", `{"title":"line\nbreak"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a title with a newline: %d %s", w.Code, w.Body.String())
	}
	if w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+id+"/title", `{"title":"`+strings.Repeat("x", 121)+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a title past the limit: %d %s", w.Code, w.Body.String())
	}
	if w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+id+"/title", `{"title":"`+strings.Repeat("é", 120)+`"}`); w.Code != http.StatusOK {
		t.Fatalf("a title at the limit in runes: %d %s", w.Code, w.Body.String())
	}

	hold := make(chan struct{})
	a.mu.Lock()
	a.hold = hold
	a.mu.Unlock()
	if w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"two"}`); w.Code != http.StatusAccepted {
		t.Fatalf("message: %d %s", w.Code, w.Body.String())
	}
	waitCalls(t, a, 2)
	w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+id+"/title", `{"title":"mid turn"}`)
	close(hold)
	if w.Code != http.StatusConflict {
		t.Fatalf("a rename mid-turn: %d %s", w.Code, w.Body.String())
	}
	events := turnsEnded(t, st, id, 2)
	if got := renames(events); len(got) != 1 {
		t.Fatalf("refused renames were recorded: %+v", got)
	}
}

// waitCalls waits until the scripted model has been asked n times.
func waitCalls(t *testing.T, m *modelServer, n int32) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); m.calls.Load() < n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the model was asked %d times, want %d", m.calls.Load(), n)
		}
	}
}

// A title with a format character, which some clients draw as nothing or use
// to reorder the text, is refused.
func TestTitleRefusesFormatCharacters(t *testing.T) {
	for _, raw := range []string{"fix\u202etxt.exe", "a\u200bb", "\ufeffname", "x\u2066y"} {
		if title, msg := cleanTitle(raw); msg == "" {
			t.Errorf("cleanTitle(%q) = %q, want refused", raw, title)
		}
	}
	for _, raw := range []string{"a\u200db", "x\u200d\U0001F4BB", "\U0001F468\u200d", "soft\u00adhyphen"} {
		if title, msg := cleanTitle(raw); msg == "" {
			t.Errorf("cleanTitle(%q) = %q, want refused", raw, title)
		}
	}
	for _, raw := range []string{"Retry fix, take 2 ✓", "pairing \U0001F468\u200d\U0001F4BB", "\u2764\ufe0f\u200d\U0001F525 hot fix", "\U0001F44B\U0001F3FD\u200d\u2642"} {
		if title, msg := cleanTitle(raw); msg != "" || title != raw {
			t.Errorf("cleanTitle(%q) = %q %q, want it kept", raw, title, msg)
		}
	}
}
