package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A Studio session offers the subagent tools with what a parallel request
// means, /parallel sends the work as that request, and the start shows as
// one card naming each part, then a card per subagent.
func TestStudioParallelSubagents(t *testing.T) {
	r := newStudioRig(t, "",
		callTool("t1", "tasks", map[string]any{"tasks": []any{
			map[string]any{"prompt": "child a: read the readme", "description": "part a"},
			map[string]any{"prompt": "child b: list the files", "description": "part b"},
		}}),
		say("child done"), say("child done"), say("both parts reported"))
	id := r.open()
	stop, ups := r.prompt(id, "/parallel read the readme, list the files")
	if stop != "end_turn" {
		t.Fatalf("stop %q", stop)
	}

	r.model.mu.Lock()
	first := r.model.bodies[0]
	r.model.mu.Unlock()
	for _, want := range []string{`"name":"tasks"`, `"name":"task"`, "AT THE SAME TIME", "Calls in ONE response run concurrently"} {
		if !strings.Contains(first, want) {
			t.Errorf("the model's first request lacks %q", want)
		}
	}

	got, _ := r.recorded(id, agent.EvCommandInvoked)
	if len(got) != 1 || got[0]["name"] != "/parallel" || got[0]["source"] != "builtin" {
		t.Fatalf("command.invoked: %v", got)
	}
	msgs, _ := r.recorded(id, agent.EvUserMessage)
	if len(msgs) != 1 || msgs[0]["text"] != agent.ParallelPrompt("read the readme, list the files") {
		t.Fatalf("the work was not sent as a parallel request: %v", msgs)
	}

	titles := map[string]bool{}
	for _, u := range ups {
		if u["sessionUpdate"] == "tool_call" {
			if s, _ := u["title"].(string); s != "" {
				titles[s] = true
			}
		}
	}
	for _, want := range []string{"tasks: 2 in parallel: part a · part b", "subagent general: part a", "subagent general: part b"} {
		if !titles[want] {
			t.Errorf("no card titled %q among %v", want, titles)
		}
	}

	// With nothing to split, /parallel says what it needs and runs nothing.
	_, ups = r.prompt(id, "/parallel")
	if !strings.Contains(textOf(ups), "give the work to split") {
		t.Fatalf("/parallel alone said %q", textOf(ups))
	}
	if msgs, _ := r.recorded(id, agent.EvUserMessage); len(msgs) != 1 {
		t.Fatalf("/parallel alone sent a message: %v", msgs)
	}
}

// In the terminal, /parallel sends the work as a request to start its parts
// as subagents at once.
func TestCLIParallelCommand(t *testing.T) {
	c := startCLI(t)
	conv := c.task("/parallel review pkg/auth, test pkg/store")
	want, _ := json.Marshal(agent.ParallelPrompt("review pkg/auth, test pkg/store"))
	if !strings.Contains(conv, string(want[1:len(want)-1])) {
		t.Fatalf("the model was not sent the parallel request: %s", conv)
	}
	// Its @ mentions are attached as in any message.
	if err := os.WriteFile(filepath.Join(c.ws, "plan.md"), []byte("STEP-ONE-MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	if conv := c.task("/parallel follow @plan.md for pkg/auth and pkg/store"); !strings.Contains(conv, "STEP-ONE-MARKER") ||
		!strings.Contains(conv, "Run this work as parallel subagents") {
		t.Fatalf("the mention was not attached to the parallel turn: %s", conv)
	}
}

// headlessEvents runs -p with stream-json output and returns its events by type.
func headlessEvents(t *testing.T, stdin string, args ...string) (map[string][]map[string]any, error, string) {
	t.Helper()
	m := &bgModelServer{}
	ws := bgWorkspace(t, m.start(t), "")
	cmd := mainHelper(append([]string{"-C", ws, "-output-format", "stream-json"}, args...))
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	evs := map[string][]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var ev struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil {
			evs[ev.Type] = append(evs[ev.Type], ev.Payload)
		}
	}
	return evs, err, errOut.String()
}

// -p /parallel sends the parallel turn and records the command, as the
// terminal and Studio do; with no work it says so and sends nothing; a
// stream-json line runs it too.
func TestHeadlessParallelCommand(t *testing.T) {
	evs, err, stderr := headlessEvents(t, "", "-p", "/parallel review pkg/auth, test pkg/store")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if c := evs["command.invoked"]; len(c) != 1 || c[0]["name"] != "/parallel" || c[0]["args"] != "review pkg/auth, test pkg/store" {
		t.Fatalf("command.invoked: %v", c)
	}
	if u := evs["user.message"]; len(u) != 1 || u[0]["text"] != agent.ParallelPrompt("review pkg/auth, test pkg/store") || u[0]["harness_note"] != nil {
		t.Fatalf("user.message: %v", u)
	}

	evs, err, stderr = headlessEvents(t, "", "-p", "/parallel")
	if err == nil || len(evs["user.message"]) != 0 || !strings.Contains(stderr+fmt.Sprint(evs["result"]), "needs the work to split") {
		t.Fatalf("/parallel alone: err %v, events %v, stderr %s", err, evs, stderr)
	}

	line := `{"type":"user","message":{"role":"user","content":"/parallel review pkg/auth, test pkg/store"}}` + "\n"
	evs, err, stderr = headlessEvents(t, line, "-p", "-input-format", "stream-json")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if c := evs["command.invoked"]; len(c) != 1 || c[0]["name"] != "/parallel" {
		t.Fatalf("stream-json /parallel: %v", c)
	}
}

// The note is judged on the task given after -p, never on piped stdin.
func TestHeadlessNoteOnlyOnTheTypedTask(t *testing.T) {
	evs, err, stderr := headlessEvents(t, "the workers run in parallel\n", "-p", "summarise this")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if u := evs["user.message"]; len(u) != 1 || u[0]["harness_note"] != nil || !strings.Contains(fmt.Sprint(u[0]["text"]), "the workers run in parallel") {
		t.Fatalf("piped stdin raised the note, or was not piped: %v", u)
	}
	evs, err, stderr = headlessEvents(t, "", "-p", "review pkg/auth and pkg/store in parallel")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if u := evs["user.message"]; len(u) != 1 || u[0]["harness_note"] != agent.ParallelNote {
		t.Fatalf("the typed request went without the note: %v", u)
	}
	// A -p run cannot answer a question, so a vague request gets the note
	// that holds it to a plan or the question, with no code edits.
	evs, err, stderr = headlessEvents(t, "", "-p", "handle the three tickets at the same time")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if u := evs["user.message"]; len(u) != 1 || u[0]["harness_note"] != agent.UnattendedClarifyNote {
		t.Fatalf("a -p run did not get the unattended note: %v", u)
	}
}

// In Studio, an attached resource that speaks of parallel work raises no
// note; the person's own words do.
func TestStudioNoteOnlyOnTheTypedPrompt(t *testing.T) {
	r := newStudioRig(t, "", say("ok"), say("ok"), say("ok"))
	r.write("notes.md", "The workers run in parallel.")
	id := r.open()
	if res := r.promptBlocks(id, map[string]any{"type": "resource_link", "name": "notes.md",
		"uri": "file://" + filepath.Join(r.ws, "notes.md")}); res.Error != nil {
		t.Fatalf("%+v", res.Error)
	}
	r.model.mu.Lock()
	sentFile := strings.Contains(strings.Join(r.model.bodies, "\n"), "run in parallel")
	r.model.mu.Unlock()
	if !sentFile {
		t.Fatal("the attached file did not reach the model, so nothing was tried")
	}
	r.prompt(id, "review pkg/auth and pkg/store in parallel")
	// In Studio the person can answer, so an unclear request gets the note that asks.
	r.prompt(id, "handle the three tickets at the same time")
	msgs, _ := r.recorded(id, agent.EvUserMessage)
	if len(msgs) != 3 || msgs[0]["harness_note"] != nil || msgs[1]["harness_note"] != agent.ParallelNote ||
		msgs[2]["harness_note"] != agent.ClarifyNote {
		t.Fatalf("user.message notes: %v", msgs)
	}
}

// A wait for the model shows in Studio: on the subagent's card, or as a
// thought for the session's own request.
func TestStudioShowsModelWaiting(t *testing.T) {
	c, s := &acpConn{}, &acpSession{}
	ev := func(w agent.ModelWaiting) agent.Event {
		b, _ := json.Marshal(w)
		return agent.Event{Type: agent.EvModelWaiting, Payload: b}
	}
	own := c.updates(s, ev(agent.ModelWaiting{Ahead: 2}), false)
	if len(own) != 1 || own[0]["sessionUpdate"] != "agent_thought_chunk" || !strings.Contains(fmt.Sprint(own[0]["content"]), "waiting for the model (2 ahead)") {
		t.Fatalf("the session's own wait: %v", own)
	}
	child := c.updates(s, ev(agent.ModelWaiting{Session: "c1", Ahead: 1}), false)
	if len(child) != 1 || child[0]["sessionUpdate"] != "tool_call_update" || child[0]["toolCallId"] != "sub-c1" {
		t.Fatalf("a subagent's wait: %v", child)
	}
	if len(c.updates(s, ev(agent.ModelWaiting{Ahead: 2}), true)) != 0 {
		t.Fatal("a replay shows a wait that is over")
	}
}
