package ui

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// diagnosticsEvents are a write and the diagnostics server's start inside
// it, in the order the record holds them: the start is requested and
// approved while the write runs, the write's result follows, and the
// start's outcome comes once the server is up.
func diagnosticsEvents(t *testing.T) []agent.Event {
	sys := func(ev agent.Event) agent.Event { ev.Actor = agent.ActorSystem; return ev }
	write := event(t, agent.EvActionRequested, agent.ActionRequested{CallID: "w1", Tool: "write", Args: []byte(`{"path":"a.go","content":"package a\n"}`)})
	write.Actor = agent.ActorAgent
	return []agent.Event{
		write,
		sys(event(t, agent.EvActionRequested, agent.ActionRequested{CallID: "diag-go-1", Tool: "bash", Via: agent.DiagnosticsVia, Args: []byte(`{"command":"gopls"}`)})),
		sys(event(t, agent.EvActionApproved, map[string]string{"call_id": "diag-go-1", "by": agent.ByPolicy, "rule": "bash(gopls)", "step": "allow"})),
		event(t, agent.EvObservation, agent.Observation{CallID: "w1", Tool: "write",
			Content: "Created a.go (10 bytes, 1 lines).\n\n[gopls diagnostics for a.go: none]"}),
		sys(event(t, agent.EvObservation, agent.Observation{CallID: "diag-go-1", Tool: "bash",
			Content: "language server gopls started for diagnostics (allowed by policy: bash(gopls))"})),
	}
}

// The engine's start of a diagnostics server is drawn as a note, never as
// a Bash call of the agent's: no tool header, no "Running Bash", no rule
// approval line, and the write's result stays under the write.
func TestDiagnosticsStartIsAnEngineNote(t *testing.T) {
	g := newRig(t, 100, 40)
	r := NewRenderer(io.Discard, false)
	r.Attach(g.lr)
	evs := diagnosticsEvents(t)
	for i, ev := range evs {
		r.Event(ev)
		if i == 1 && r.dock.act.label != "Running Write" {
			t.Fatalf("the start took the activity label: %q", r.dock.act.label)
		}
	}
	g.settle()
	g.waitText("started for diagnostics")
	screen := g.term.Text()
	if strings.Contains(screen, "Bash") || strings.Contains(screen, "auto: bash(gopls)") {
		t.Fatalf("the start was drawn as a call:\n%s", screen)
	}
	w, c, n := strings.Index(screen, "Write"), strings.Index(screen, "Created a.go"), strings.Index(screen, "diagnostics: language server gopls started")
	if w < 0 || c < w || n < c {
		t.Fatalf("want the write, its result, then the note:\n%s", screen)
	}

	var out bytes.Buffer
	lr := NewRenderer(&out, false)
	for _, ev := range evs {
		lr.Event(ev)
	}
	if text := out.String(); strings.Contains(text, "● bash") || !strings.Contains(text, "diagnostics: language server gopls started") {
		t.Fatalf("line view:\n%s", text)
	}

	// A refused start is a note too.
	out.Reset()
	lr = NewRenderer(&out, false)
	req := event(t, agent.EvActionRequested, agent.ActionRequested{CallID: "diag-go-2", Tool: "bash", Via: agent.DiagnosticsVia, Args: []byte(`{"command":"gopls"}`)})
	req.Actor = agent.ActorSystem
	lr.Event(req)
	lr.Event(event(t, agent.EvActionDenied, map[string]string{"call_id": "diag-go-2", "reason": "nobody is asked for it: requires confirmation", "step": "default"}))
	if text := out.String(); strings.Contains(text, "✕") || !strings.Contains(text, "language server not started: nobody is asked") {
		t.Fatalf("refused start:\n%s", text)
	}
}

// A denied call is over: a diagnostics note after it is drawn, not held
// behind it forever; and a note held behind a call that never returns is
// drawn when the run ends.
func TestDiagnosticsNoteAfterADeniedCall(t *testing.T) {
	g := newRig(t, 100, 40)
	r := NewRenderer(io.Discard, false)
	r.Attach(g.lr)
	sys := func(ev agent.Event) agent.Event { ev.Actor = agent.ActorSystem; return ev }
	agentCall := func(id, cmd string) agent.Event {
		ev := event(t, agent.EvActionRequested, agent.ActionRequested{CallID: id, Tool: "bash", Args: []byte(`{"command":"` + cmd + `"}`)})
		ev.Actor = agent.ActorAgent
		return ev
	}
	start := func(id, text string) []agent.Event {
		return []agent.Event{
			sys(event(t, agent.EvActionRequested, agent.ActionRequested{CallID: id, Tool: "bash", Via: agent.DiagnosticsVia, Args: []byte(`{"command":"gopls"}`)})),
			sys(event(t, agent.EvActionApproved, map[string]string{"call_id": id, "by": agent.ByPolicy})),
			sys(event(t, agent.EvObservation, agent.Observation{CallID: id, Tool: "bash", Content: text})),
		}
	}
	evs := []agent.Event{
		agentCall("b1", "rm -rf x"),
		sys(event(t, agent.EvActionDenied, map[string]string{"call_id": "b1", "reason": "denied by rule bash(rm *)", "step": "deny", "by": agent.ByPolicy})),
	}
	evs = append(evs, start("diag-go-1", "language server gopls started for diagnostics (first)")...)
	// A call that never returns holds the next note until the run ends.
	evs = append(evs, agentCall("b2", "sleep 100"))
	evs = append(evs, start("diag-py-2", "language server basedpyright-langserver started for diagnostics (second)")...)
	for _, ev := range evs {
		r.Event(ev)
	}
	g.settle()
	g.waitText("(first)")
	if strings.Contains(g.term.Text(), "(second)") {
		t.Fatalf("a note was drawn inside a call in flight:\n%s", g.term.Text())
	}
	r.Event(sys(event(t, agent.EvSessionEnded, agent.SessionEnded{Reason: agent.TermUserInterrupt})))
	g.settle()
	g.waitText("(second)")
}
