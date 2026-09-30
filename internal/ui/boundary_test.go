package ui

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// hostile carries every way text has been found to reach a terminal raw:
// OSC 52 (clipboard), OSC 0 (title), OSC 8 (link), ED and CUP, C1 CSI and
// OSC, DCS, BEL, and the same behind a zero-width joiner.
const hostile = "A\x1b]52;c;U1BPT0Y=\aB\x1b]0;TITLE\aC\x1b[2JD\x1b[5;5HE\u009b31mF\u009d0;T\aG\x1b]8;;http://x\x1b\\H\x1bP1$r\x1b\\I‍\x1b]0;Z\aJ‍\r\bK"

// forbidden are the bytes none of that may leave on the wire.
var forbidden = []string{"\x1b]", "\a", "\x1b[2J", "\x1b[5;5H", "\u009b", "\u009d", "\x1bP", "\x1b\\", "\b"}

func assertClean(t *testing.T, what, wire string) {
	t.Helper()
	for _, f := range forbidden {
		if strings.Contains(wire, f) {
			i := strings.Index(wire, f)
			t.Errorf("%s: %q reached the terminal: …%q…", what, f, wire[max(0, i-30):min(len(wire), i+30)])
		}
	}
}

func event(t *testing.T, typ agent.EventType, payload any) agent.Event {
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return agent.Event{Type: typ, Payload: raw}
}

// No model-chosen string reaches the terminal with anything but text and
// colour: not through a tool's arguments, a result, a notice, a reply, a
// dialog, a pick, the footer or a block a command appends.
func TestNothingButTextReachesTheTerminal(t *testing.T) {
	g := newRig(t, 100, 40)
	clock := newFakeClock()
	tm := &timers{clock: clock}
	g.lr.d.mu.Lock()
	g.lr.d.now, g.lr.d.after = clock.Now, tm.after
	g.lr.d.mu.Unlock()
	r := NewRenderer(io.Discard, false)
	r.Attach(g.lr)
	r.SetWorkspace("/ws")

	// Tool calls and their results, as each tool's arguments.
	for i, c := range []struct {
		tool string
		args map[string]any
	}{
		{"bash", map[string]any{"command": hostile, "description": hostile}},
		{"edit", map[string]any{"path": "/ws/" + hostile, "old_string": hostile, "new_string": hostile + "2"}},
		{"write", map[string]any{"path": "/ws/" + hostile, "content": hostile}},
		{"grep", map[string]any{"pattern": hostile, "path": "/ws/" + hostile}},
		{"task", map[string]any{"description": hostile}},
		{"web_fetch", map[string]any{"url": hostile}},
		{hostile, map[string]any{"x": hostile}},
	} {
		id := "c" + string(rune('0'+i))
		args, _ := json.Marshal(c.args)
		r.Event(event(t, agent.EvActionRequested, agent.ActionRequested{CallID: id, Tool: c.tool, Args: args}))
		if c.tool == "edit" || c.tool == "write" {
			r.Checkpoint(nil)("/ws/"+hostile, []byte(hostile), true)
		}
		code := 1
		r.Event(event(t, agent.EvObservation, agent.Observation{CallID: id, Tool: c.tool, Content: "exit 1 · 1ms\n" + hostile, ExitCode: &code}))
	}
	r.Event(event(t, agent.EvObservation, agent.Observation{CallID: "e", Tool: "read", Content: hostile, IsError: true}))
	r.Event(event(t, agent.EvActionDenied, map[string]string{"call_id": "zz", "reason": hostile}))
	r.Event(event(t, agent.EvSubagentNotice, agent.Notice{TaskID: hostile, Description: hostile, Status: hostile}))
	r.Event(event(t, agent.EvAgentDelta, agent.Delta{Text: hostile}))
	r.Event(event(t, agent.EvAgentMessage, agent.Message{Text: hostile}))
	r.Event(event(t, agent.EvAgentReasoning, agent.Message{Text: hostile}))
	r.Event(event(t, agent.EvModelCall, agent.ModelCall{Model: hostile, TokensIn: 10, ContextWindow: 100}))

	// Blocks a command appends, and the footer.
	g.lr.Append(Block{Kind: BlockTable, Rows: [][]string{{hostile, "b"}, {"c", hostile}}})
	for _, k := range []BlockKind{BlockNotice, BlockError, BlockMarkdown, BlockDiff, BlockToolOut} {
		g.lr.Append(Block{Kind: k, Text: hostile, Path: hostile})
	}
	g.lr.SetStatus(StatusModel{Model: hostile, Provider: hostile, Mode: hostile, GitBranch: hostile, Cwd: hostile, Line: hostile, ContextTokens: 5})
	g.lr.Notify(Toast{Text: hostile})

	// Dialogs: the approver's, for an edit to a hostile path with a scope,
	// and a hand-made one with every text field hostile.
	a := &DialogApprover{Base: NewApprover(io.Discard), Reader: g.lr, Render: r}
	args, _ := json.Marshal(map[string]string{"path": "/ws/" + hostile, "old_string": "x", "new_string": hostile})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.Approve(context.Background(), "edit", args, policy.Result{Decision: policy.Ask, Step: "default",
			Scope: "edit(/ws/" + hostile + ")", Reason: hostile})
	}()
	g.waitText("Make this edit to")
	clock.advance(time.Second)
	g.keys("2")
	g.settle()
	tm.advance(approvalGuard)
	<-done
	go func() {
		_, _ = g.lr.Dialog(context.Background(), DialogSpec{Kind: DialogChoice, Title: hostile, Why: hostile, Ask: "Q" + hostile,
			Choices: []Choice{{ID: "a", Label: hostile}, {ID: "b", Label: hostile}}, Default: "a",
			Outcome: func(string) string { return hostile }})
	}()
	g.waitText("QA")
	clock.advance(time.Second)
	g.keys("1")
	g.settle()
	tm.advance(approvalGuard)
	g.settle()
	go func() {
		_, _ = g.lr.Pick(context.Background(), PickSpec{Title: hostile, Items: []PickItem{{ID: "x", Label: hostile, Detail: hostile}}})
	}()
	g.settle()
	g.keys("\x03")
	g.settle()

	g.out.mu.Lock()
	wire := g.out.log.String()
	g.out.mu.Unlock()
	assertClean(t, "the dock", wire)
	if !strings.Contains(g.term.All(), "ABCDE") || !strings.Contains(g.term.All(), "GHIJK") {
		t.Errorf("the text itself was lost:\n%s", g.term.All())
	}
}
