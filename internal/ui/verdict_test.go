package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func ev(t agent.EventType, p any) agent.Event {
	b, _ := json.Marshal(p)
	return agent.Event{Type: t, Payload: b}
}

// The answer tool's call is not drawn; its verdict is, with a contradicted
// claim struck through beside the evidence against it.
func TestVerdictIsDrawnAndAnswerCallHidden(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out, false)
	r.Event(ev(agent.EvActionRequested, agent.ActionRequested{CallID: "a1", Tool: agent.AnswerToolName, Args: []byte(`{"text":"x"}`)}))
	r.Event(ev(agent.EvObservation, agent.Observation{CallID: "a1", Tool: agent.AnswerToolName, Content: "Answer received"}))
	if out.Len() != 0 {
		t.Fatalf("the answer call was drawn: %q", out.String())
	}
	v := agent.AnswerVerdict{Verified: 1, Contradicted: 1, Unverified: 1, Claims: []agent.ClaimVerdict{
		{N: 1, Text: "The service listens on 8080.", Verdict: agent.ClaimVerified, LoadBearing: true},
		{N: 2, Text: "All 15 tests pass.", Verdict: agent.ClaimContradicted, LoadBearing: true, Conflict: &agent.Conflict{Seq: 7, Excerpt: "12 tests passed"}},
		{N: 3, Text: "Go is compiled.", Verdict: agent.ClaimUnverified},
	}}
	r.Event(ev(agent.EvAnswerVerdict, v))
	got := out.String()
	for _, want := range []string{"1 verified · 1 unverified (general knowledge) · 1 contradicted", "✗ 2 All 15 tests pass.", "#7: 12 tests passed"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "/evidence") {
		t.Error("-p output has no /evidence to point at")
	}
	if lv, ok := r.LastVerdict(); !ok || lv.Contradicted != 1 {
		t.Fatal("the renderer keeps the last verdict")
	}
	rows := VerdictRows(Style{enabled: true}, v, true)
	if !strings.Contains(strings.Join(rows, "\n"), "\x1b[9m") {
		t.Error("a contradicted claim is struck through")
	}
	if VerdictRows(Style{}, agent.AnswerVerdict{}, true) != nil {
		t.Error("an answer with nothing to check draws nothing")
	}
}
