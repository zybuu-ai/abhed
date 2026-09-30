package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A denial's reason can carry a hook's or a model's text; control and
// format characters in it are shown, never obeyed, and it stays on one line.
func TestDeniedReasonIsShownVisibly(t *testing.T) {
	var out strings.Builder
	r := NewRenderer(&out, false)
	payload, _ := json.Marshal(map[string]string{"reason": "File not found: a\r\x1b[2K✓ approved‮\nfake line"})
	r.Event(agent.Event{Type: agent.EvActionDenied, Payload: payload})
	got := out.String()
	if strings.ContainsAny(got, "\r\x1b‮") || strings.Count(strings.TrimRight(got, "\n"), "\n") != 0 {
		t.Fatalf("the reason drove the terminal: %q", got)
	}
}
