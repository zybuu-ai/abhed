package server

import (
	"encoding/json"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// The stream closes after the last event a session makes: an end that owes
// no background work and, when it announced a suggestion, that suggestion's
// model.call, even when the settled end comes between the two.
func TestStreamEndAwaitsTheSuggestionAcrossTheSettledEnd(t *testing.T) {
	end := func(p agent.SessionEnded) agent.Event {
		raw, _ := json.Marshal(p)
		return agent.Event{Type: agent.EvSessionEnded, Payload: raw}
	}
	sugg := func() agent.Event {
		raw, _ := json.Marshal(agent.ModelCall{Purpose: agent.PurposeSuggestion})
		return agent.Event{Type: agent.EvModelCall, Payload: raw}
	}
	idle := func() bool { return false }
	for _, c := range []struct {
		name  string
		evs   []agent.Event
		close []bool
	}{
		{"an end owing nothing", []agent.Event{end(agent.SessionEnded{})}, []bool{true}},
		{"an end and its suggestion", []agent.Event{end(agent.SessionEnded{Suggesting: true}), sugg()}, []bool{false, true}},
		{"the settled end before the suggestion",
			[]agent.Event{end(agent.SessionEnded{Background: 1, Suggesting: true}), end(agent.SessionEnded{Settled: true}), sugg()},
			[]bool{false, false, true}},
		{"the suggestion before the settled end",
			[]agent.Event{end(agent.SessionEnded{Background: 1, Suggesting: true}), sugg(), end(agent.SessionEnded{Settled: true})},
			[]bool{false, false, true}},
		{"a later run's end with no suggestion",
			[]agent.Event{end(agent.SessionEnded{Background: 1, Suggesting: true}), end(agent.SessionEnded{})},
			[]bool{false, true}},
	} {
		var st streamEnd
		for i, e := range c.evs {
			if got := st.closes(e, idle); got != c.close[i] {
				t.Errorf("%s: event %d closes = %v, want %v", c.name, i, got, c.close[i])
			}
		}
	}
}
