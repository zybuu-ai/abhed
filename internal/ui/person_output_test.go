package ui

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A person's own command, such as a ! line, shows its output under the call
// once and in full up to a screen's worth; an agent's call stays folded.
func TestPersonCommandOutputShownOnceUnderItsCall(t *testing.T) {
	g := newRig(t, 100, 60)
	r := NewRenderer(io.Discard, false)
	r.Attach(g.lr)
	if !r.DrawsToolOutput() {
		t.Fatal("a docked renderer does not say it draws tool output")
	}
	var body []string
	for i := 1; i <= 12; i++ {
		body = append(body, fmt.Sprintf("row-%02d", i))
	}
	code := 0
	for _, c := range []struct {
		id    string
		actor agent.Actor
	}{{"bang-1", agent.ActorUser}, {"c-1", agent.ActorAgent}} {
		req := event(t, agent.EvActionRequested, agent.ActionRequested{CallID: c.id, Tool: "bash", Args: []byte(`{"command":"seq 12"}`)})
		req.Actor = c.actor
		r.Event(req)
		r.Event(event(t, agent.EvObservation, agent.Observation{CallID: c.id, Tool: "bash",
			Content: "exit 0 · 1ms\n" + strings.Join(body, "\n"), ExitCode: &code}))
		g.settle()
	}
	g.waitText("ctrl+o to expand")
	screen := g.term.Text()
	// The person's call shows every row; the agent's shows three and the last two.
	if n := strings.Count(screen, "row-05"); n != 1 {
		t.Fatalf("row-05 shown %d times:\n%s", n, screen)
	}
	if n := strings.Count(screen, "row-12"); n != 2 {
		t.Fatalf("row-12 shown %d times:\n%s", n, screen)
	}
}

func TestLineRendererDoesNotDrawToolOutput(t *testing.T) {
	if NewRenderer(io.Discard, false).DrawsToolOutput() {
		t.Fatal("the line view draws only a summary")
	}
}
