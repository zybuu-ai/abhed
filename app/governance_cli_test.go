package app

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// eventsOf are the payloads of the events of type t, decoded into T.
func eventsOf[T any](t *testing.T, evs []agent.Event, typ agent.EventType) []T {
	t.Helper()
	var out []T
	for _, ev := range evs {
		if ev.Type != typ {
			continue
		}
		var p T
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// Through the CLI: a mode chosen before the first message lands in that
// conversation's record ahead of it, /mode auto takes a typed yes, and a
// refused answer changes nothing.
func TestCLIModeChangesReachTheRecord(t *testing.T) {
	c := startCLI(t)
	c.command("/mode plan", "mode: plan")
	c.command("/mode auto", "answer 1-2")
	c.command("no", "mode stays plan")
	c.command("/mode auto", "answer 1-2")
	c.command("yes", "mode: auto")
	c.task("hello")
	evs := c.export()
	var got []string
	for _, mc := range eventsOf[agent.ModeChanged](t, evs, agent.EvModeChanged) {
		got = append(got, mc.From+">"+mc.To+"/"+mc.Via)
	}
	if want := []string{"default>plan/slash", "plan>auto/slash"}; !slices.Equal(got, want) {
		t.Fatalf("mode changes %v, want %v", got, want)
	}
	types := (func() []agent.EventType {
		var ts []agent.EventType
		for _, e := range evs {
			ts = append(ts, e.Type)
		}
		return ts
	})()
	if first := slices.Index(types, agent.EvUserMessage); first < slices.Index(types, agent.EvModeChanged) {
		t.Fatalf("the mode change was recorded after the message it preceded: %v", types)
	}
}
