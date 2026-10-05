package ui

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Each kind of harness activity draws one line in the transcript, in words.
func TestActivityLinesAreDrawn(t *testing.T) {
	cases := []struct {
		name string
		ev   agent.Event
		want []string
	}{
		{"spawned", event(t, agent.EvSubagentSpawned, map[string]any{"description": "find the loader", "agent_type": "explore", "session": "c1", "model": "stub-1"}),
			[]string{"├ explore started · find the loader · stub-1"}},
		{"compacting", event(t, agent.EvCompactStarted, agent.Compaction{BeforeTokens: 12345, Trigger: "auto"}),
			[]string{"◆ compacting the conversation · 12k tokens"}},
		{"compacted", event(t, agent.EvCompactDone, agent.Compaction{BeforeTokens: 12345, AfterTokens: 2100, Trigger: "auto"}),
			[]string{"◆ compacted · 12k → 2.1k tokens (auto)"}},
		{"compaction failed", event(t, agent.EvCompactDone, map[string]string{"error": "model down"}),
			[]string{"◆ compaction failed: model down"}},
		{"mode", event(t, agent.EvModeChanged, agent.ModeChanged{From: "default", To: "plan", By: "user", Via: "slash"}),
			[]string{"◆ mode: default → plan · slash"}},
		{"rule", event(t, agent.EvPermissionChanged, agent.PermissionChanged{Op: "add", List: "allow", Rule: "bash(go test*)", Scope: "session"}),
			[]string{"◆ session allow rule added: bash(go test*) (until /clear)"}},
		{"hook", event(t, agent.EvHookFired, agent.HookFired{Extension: "guard", Event: "pre_tool", Verdict: "block"}),
			[]string{"◆ hook guard · pre_tool · block"}},
		{"auto", event(t, agent.EvActionApproved, map[string]string{"call_id": "1", "by": "policy", "step": "allow", "rule": "bash(go test*)"}),
			[]string{"✓ auto: bash(go test*)"}},
	}
	for _, c := range cases {
		var out strings.Builder
		NewRenderer(&out, false).Event(c.ev)
		for _, w := range c.want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%s: want %q in %q", c.name, w, out.String())
			}
		}
	}
}

// What is not a rule's approval draws nothing: a person's answer, or a read
// the default lets through.
func TestAutoLineOnlyForARule(t *testing.T) {
	for _, m := range []map[string]string{
		{"by": "policy", "step": "default"},
		{"by": "reviewer", "rule": "bash(x)"},
		{"by": "user"},
	} {
		var out strings.Builder
		NewRenderer(&out, false).Event(event(t, agent.EvActionApproved, m))
		if out.Len() != 0 {
			t.Errorf("%v drew %q", m, out.String())
		}
	}
	var out strings.Builder
	NewRenderer(&out, false).Event(event(t, agent.EvModeChanged, agent.ModeChanged{From: "default", To: "plan", Via: "carried"}))
	if out.Len() != 0 {
		t.Errorf("a carried mode drew %q", out.String())
	}
}

// A returned subagent is named by the type it was started as.
func TestSubagentTreeNamesTheReturn(t *testing.T) {
	var out strings.Builder
	r := NewRenderer(&out, false)
	r.Event(event(t, agent.EvSubagentSpawned, map[string]any{"description": "d", "agent_type": "review", "session": "c9"}))
	r.Event(event(t, agent.EvSubagentReturn, map[string]any{"description": "d", "session": "c9", "reason": "completed", "turns": 3, "tokens_in": 1000, "tokens_out": 200}))
	if !strings.Contains(out.String(), "└ review returned · completed · 3 turns · 1.2k tokens · d") {
		t.Fatalf("no return line: %q", out.String())
	}
}

// Text an agent or extension chose cannot drive the terminal from an
// activity line, and stays on one line.
func TestActivityTextIsShownNotObeyed(t *testing.T) {
	evil := "x\x1b]0;PWNED\x07\r\x1b[2K✓ approved\u202e\nfake"
	for _, ev := range []agent.Event{
		event(t, agent.EvSubagentSpawned, map[string]any{"description": evil, "agent_type": evil, "model": evil}),
		event(t, agent.EvHookFired, agent.HookFired{Extension: evil, Event: evil, Verdict: evil}),
		event(t, agent.EvActionApproved, map[string]string{"by": "policy", "rule": evil}),
		event(t, agent.EvCompactDone, map[string]string{"error": evil}),
		event(t, agent.EvTodoUpdated, agent.TodoList{Items: []agent.Todo{{ID: "1", Text: evil, Status: "pending"}}}),
	} {
		var out strings.Builder
		NewRenderer(&out, false).Event(ev)
		got := out.String()
		if strings.ContainsAny(got, "\r\x1b\a\u202e") || strings.Count(strings.TrimRight(got, "\n"), "\n") != 0 {
			t.Errorf("%s drove the terminal: %q", ev.Type, got)
		}
	}
}

// The todo list is drawn whole, each item with its state.
func TestTodoChecklist(t *testing.T) {
	var out strings.Builder
	r := NewRenderer(&out, false)
	r.Event(event(t, agent.EvTodoUpdated, agent.TodoList{Items: []agent.Todo{
		{ID: "1", Text: "read the file", Status: "done"},
		{ID: "2", Text: "change the greeting", Status: "in_progress"},
		{ID: "3", Text: "run the tests", Status: "pending"},
	}}))
	got := out.String()
	for _, w := range []string{"⎿  ☒ read the file", "◼ change the greeting", "☐ run the tests"} {
		if !strings.Contains(got, w) {
			t.Errorf("want %q in %q", w, got)
		}
	}
	if sum, open := todoSummary(r.Todos()); !open || sum != "1/3 done · change the greeting" {
		t.Errorf("summary %q %v", sum, open)
	}
	out.Reset()
	if !r.ShowTodos() || !strings.Contains(out.String(), "Todos · 1/3 done") {
		t.Errorf("/todos drew %q", out.String())
	}
}

// On the dock the open list sits above the input as one line, and Ctrl-T
// lists every item.
func TestDockTodoSummaryAndCtrlT(t *testing.T) {
	g := newRig(t, 80, 24)
	r := NewRenderer(&strings.Builder{}, false)
	r.Attach(g.lr)
	r.Event(event(t, agent.EvTodoUpdated, agent.TodoList{Items: []agent.Todo{
		{ID: "1", Text: "first step", Status: "done"},
		{ID: "2", Text: "second step", Status: "in_progress"},
		{ID: "3", Text: "third step", Status: "pending"},
	}}))
	g.waitText("Todos 1/3 done · second step · ctrl+t to list")
	g.keys("\x14")
	g.waitText("Todos 1/3 done · second step · ctrl+t to hide")
	g.waitScreen("the listed items", func(s string) bool {
		return strings.Count(s, "third step") == 2 // in the transcript and above the input
	})
	r.Event(event(t, agent.EvTodoUpdated, agent.TodoList{Items: []agent.Todo{
		{ID: "1", Text: "first step", Status: "done"},
	}}))
	g.waitScreen("the summary gone", func(s string) bool { return !strings.Contains(s, "ctrl+t to") })
}

// The title follows the state; a terminal that said it lost focus is
// signalled when an approval waits and when a turn ends, and one that is
// focused, or never said, is not. The words are always the fixed ones.
func TestAttentionTitleAndNotify(t *testing.T) {
	g := newRig(t, 80, 24)
	g.lr.SetAttention(Attention{Title: true, Notify: "bel"})
	g.settle()
	wire := func() string {
		g.out.mu.Lock()
		defer g.out.mu.Unlock()
		return g.out.log.String()
	}
	if !strings.Contains(wire(), "\x1b[22;0t\x1b]0;abhed · ready\x1b\\") {
		t.Fatalf("title not saved and set: %q", wire())
	}
	count := func() int { return strings.Count(wire(), "\a") }

	// Focus never reported: watched.
	g.lr.Attend(AttnWorking)
	g.lr.Attend(AttnReady)
	if n := count(); n != 0 {
		t.Fatalf("a terminal that never reported focus was rung %d times", n)
	}
	g.keys("\x1b[O")
	g.settle()
	g.lr.Attend(AttnWorking)
	g.lr.Attend(AttnApproval)
	g.lr.Attend(AttnWorking)
	g.lr.Attend(AttnReady)
	if n := count(); n != 2 {
		t.Fatalf("unfocused: want 2 bells (approval, turn end), got %d: %q", n, wire())
	}
	if !strings.Contains(wire(), "\x1b]0;abhed · approval needed\x1b\\") {
		t.Fatalf("no approval title: %q", wire())
	}
	g.keys("\x1b[I")
	g.settle()
	g.lr.Attend(AttnWorking)
	g.lr.Attend(AttnReady)
	if n := count(); n != 2 {
		t.Fatalf("focused again but rung: %d", n)
	}
	g.lr.Close()
	if !strings.HasSuffix(strings.SplitAfter(wire(), "\x1b[23;0t")[0], "\x1b[23;0t") {
		t.Fatalf("title not restored on close: %q", wire())
	}
}

// OSC 9 carries only the fixed words; off sends nothing.
func TestNotifySequences(t *testing.T) {
	if got := notifySeq("osc9", "abhed: turn finished"); got != "\x1b]9;abhed: turn finished\x1b\\" {
		t.Errorf("osc9: %q", got)
	}
	if got := notifySeq("off", "x"); got != "" {
		t.Errorf("off: %q", got)
	}
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	t.Setenv("TMUX", "")
	if got := notifySeq("auto", "x"); got != "\a" {
		t.Errorf("auto on an unknown terminal: %q", got)
	}
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	if got := notifySeq("auto", "x"); got != "\x1b]9;x\x1b\\" {
		t.Errorf("auto on iTerm: %q", got)
	}
	t.Setenv("TMUX", "/tmp/tmux-1/default,1,0")
	if got := notifySeq("auto", "x"); got != "\a" {
		t.Errorf("auto under tmux: %q", got)
	}
	if got := osc0("a\x07b\x1b\\c\u009cd"); got != "\x1b]0;ab\\cd\x1b\\" {
		t.Errorf("title not cleaned: %q", got)
	}
}

// /copy sends the text as base64 in OSC 52, and refuses what is too long.
func TestCopyOSC52(t *testing.T) {
	g := newRig(t, 80, 24)
	if err := g.lr.Copy("hello\nworld"); err != nil {
		t.Fatal(err)
	}
	g.settle()
	g.out.mu.Lock()
	wire := g.out.log.String()
	g.out.mu.Unlock()
	if !strings.Contains(wire, "\x1b]52;c;"+base64.StdEncoding.EncodeToString([]byte("hello\nworld"))+"\x1b\\") {
		t.Fatalf("no OSC 52: %q", wire)
	}
	if err := g.lr.Copy(strings.Repeat("x", MaxCopyBytes+1)); err == nil {
		t.Fatal("an oversized copy was sent")
	}
}

// The last reply is kept for /copy, cleaned of anything a terminal acts on.
func TestLastReplyIsKeptClean(t *testing.T) {
	r := NewRenderer(&strings.Builder{}, true)
	r.Event(event(t, agent.EvAgentMessage, agent.Message{Text: "one\x1b]52;c;eA==\x07two"}))
	if got := r.LastReply(); got != "onetwo" || !r.LastReplyCut() {
		t.Fatalf("last reply %q, cut %v", got, r.LastReplyCut())
	}
	// /copy warns when the copy is not the text the model sent.
	r.Event(event(t, agent.EvAgentMessage, agent.Message{Text: "plain text"}))
	if r.LastReplyCut() {
		t.Fatal("a reply with nothing taken out was marked as cut")
	}
	r.Event(event(t, agent.EvAgentMessage, agent.Message{Text: "ls\u202e -la"}))
	if !r.LastReplyCut() {
		t.Fatal("a reply with a bidi control was not marked as cut")
	}
}
