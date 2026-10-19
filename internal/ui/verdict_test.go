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
	for _, want := range []string{"1 verified · 1 unverified (general knowledge) · 1 contradicted", "✗ 2 All 15 tests pass.", "#7: 12 tests passed", "✗ NOT FACT: 1 statement in the reply above is contradicted"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if i, j := strings.Index(got, "NOT FACT"), strings.Index(got, "1 verified ·"); i < 0 || i > j {
		t.Error("the contradiction banner comes first, above the footer")
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

// A reply with nothing checkable draws no footer; ✓ needs something
// verified; the footer stays on one line at 80 columns.
func TestVerdictFooterRules(t *testing.T) {
	plain := agent.AnswerVerdict{Unverified: 1, Claims: []agent.ClaimVerdict{{N: 1, Text: "Hello from the stub.", Verdict: agent.ClaimUnverified}}}
	if rows := VerdictRows(Style{}, plain, true); rows != nil {
		t.Fatalf("a greeting drew %q", rows)
	}
	unk := agent.AnswerVerdict{Unverified: 1, Claims: []agent.ClaimVerdict{{N: 1, Text: "It has 9 nodes.", Verdict: agent.ClaimUnverified, LoadBearing: true}}}
	rows := VerdictRows(Style{}, unk, true)
	if len(rows) == 0 || strings.Contains(rows[0], "✓") {
		t.Fatalf("rows %q", rows)
	}
	long := agent.AnswerVerdict{Verified: 12, Unverified: 13, Contradicted: 0, Claims: make([]agent.ClaimVerdict, 25)}
	for i := range long.Claims {
		long.Claims[i] = agent.ClaimVerdict{Verdict: agent.ClaimVerified}
		if i >= 12 {
			long.Claims[i] = agent.ClaimVerdict{Verdict: agent.ClaimUnverified, LoadBearing: i%2 == 0}
		}
	}
	for _, r := range VerdictRows(Style{}, long, true) {
		if n := len([]rune(r)); strings.Contains(r, "verified ·") && n > 80 {
			t.Fatalf("footer of %d columns: %q", n, r)
		}
	}
	ok := agent.AnswerVerdict{Verified: 1, Claims: []agent.ClaimVerdict{{Verdict: agent.ClaimVerified}}}
	if rows := VerdictRows(Style{}, ok, true); len(rows) != 1 || !strings.Contains(rows[0], "✓ 1 verified") || !strings.Contains(rows[0], "/evidence N") {
		t.Fatalf("rows %q", rows)
	}
}
