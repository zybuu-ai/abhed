package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// The owner downloads the transcript as a page and the record as JSON lines,
// each as an attachment that cannot run on this origin; nobody else can.
func TestExportIsTheOwnersAttachment(t *testing.T) {
	s, st, _, _ := switchServer(t, func(c *config.Config) { c.Auth.Mode = "proxy" })
	id := sessionOf(t, callAs(t, s, "ana", "", "POST", "/v1/sessions", `{"prompt":"<script>alert(1)</script> hello"}`))
	events := turnsEnded(t, st, id, 1)

	w := callAs(t, s, "ana", "", "GET", "/v1/sessions/"+id+"/export?format=html", "")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("html export: %d %v", w.Code, w.Header())
	}
	if body := w.Body.String(); strings.Contains(body, "<script>alert(1)") || !strings.Contains(body, "&lt;script&gt;alert(1)") {
		t.Fatal("the transcript page draws the prompt as markup")
	}

	w = callAs(t, s, "ana", "", "GET", "/v1/sessions/"+id+"/export?format=jsonl", "")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("jsonl export: %d %v", w.Code, w.Header())
	}
	var lines []agent.Event
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev agent.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("a line is not an event: %v", err)
		}
		lines = append(lines, ev)
	}
	if len(lines) != len(events) || lines[0].Seq != events[0].Seq || lines[len(lines)-1].Type != agent.EvSessionEnded {
		t.Fatalf("the export holds %d events, the record %d", len(lines), len(events))
	}

	for _, c := range []struct {
		user, q string
		want    int
	}{
		{"bo", "format=html", http.StatusNotFound},
		{"bo", "format=jsonl", http.StatusNotFound},
		{"ana", "format=pdf", http.StatusBadRequest},
	} {
		if w := callAs(t, s, c.user, "", "GET", "/v1/sessions/"+id+"/export?"+c.q, ""); w.Code != c.want {
			t.Errorf("%s export %s: %d, want %d", c.user, c.q, w.Code, c.want)
		}
	}
}
