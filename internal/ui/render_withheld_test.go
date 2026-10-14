package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A message whose stored secret was withheld from the model says so on
// stderr, once, keeping stdout to the reply; a message without one says nothing.
func TestRenderSaysASecretWasWithheld(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		var out, errs bytes.Buffer
		r := NewRenderer(&out, quiet)
		r.errw = &errs
		raw, _ := json.Marshal(agent.Message{Text: "use [secret:API_TOKEN] and [secret:DB_PASS]", SecretsWithheld: "API_TOKEN,DB_PASS"})
		r.Event(agent.Event{Seq: 2, Type: agent.EvUserMessage, Payload: raw})
		got := errs.String()
		if strings.Count(got, "\n") != 1 || !strings.Contains(got, "API_TOKEN, DB_PASS") || !strings.Contains(got, "not sent to the model") {
			t.Errorf("quiet=%v: stderr %q", quiet, got)
		}
		if out.Len() != 0 {
			t.Errorf("quiet=%v: stdout %q", quiet, out.String())
		}
		errs.Reset()
		raw, _ = json.Marshal(agent.Message{Text: "hello"})
		r.Event(agent.Event{Seq: 3, Type: agent.EvUserMessage, Payload: raw})
		if errs.Len() != 0 {
			t.Errorf("quiet=%v: a plain message said %q", quiet, errs.String())
		}
	}
}
