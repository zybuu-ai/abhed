//go:build unix

package clitest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func piped(t *testing.T, o Opts) *Run {
	t.Helper()
	o.Piped = true
	h := StartRun(t, o)
	h.Wait(30 * time.Second)
	return h
}

func lastRequestText(t *testing.T, h *Run) string {
	t.Helper()
	reqs := h.Requests()
	if len(reqs) == 0 {
		t.Fatalf("no request reached the model:\n%s", h.Output())
	}
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(reqs[len(reqs)-1].Body, &body)
	var b strings.Builder
	for _, m := range body.Messages {
		b.WriteString(m.Role + ": " + m.Content + "\n")
	}
	return b.String()
}

func resultOf(t *testing.T, stdout string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var res map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &res); err != nil || res["type"] != "result" {
		t.Fatalf("the last line is not a result:\n%s", stdout)
	}
	return res
}

// cat log | abhed -p "summarise" -: the log reaches the model with the task.
func TestHeadlessStdinJoinsThePrompt(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{Args: []string{"-p", "summarise", "-"}, Stdin: "line one of the log\nline two\n", Script: `text "summary"`})
	req := lastRequestText(t, h)
	if !strings.Contains(req, "summarise") || !strings.Contains(req, "line two of the log") && !strings.Contains(req, "line two") {
		t.Fatalf("the request lacks the task or the log:\n%s", req)
	}
	if !strings.Contains(h.Stdout(), "summary") {
		t.Fatalf("stdout:\n%s", h.Stdout())
	}
}

// A task on the command line leaves inherited stdin unread, so
// `while read f; do abhed -p "fix $f"; done < list` runs once per line.
func TestHeadlessTaskLeavesStdinAlone(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{Args: []string{"-p", "fix a.go"}, Stdin: "b.go\nc.go\n", Script: `text "ok"`})
	if req := lastRequestText(t, h); !strings.Contains(req, "fix a.go") || strings.Contains(req, "c.go") {
		t.Fatalf("the rest of the list was read:\n%s", req)
	}
}

// With stdin alone, stdin is the task; flags may follow the task.
func TestHeadlessStdinIsTheTask(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{Args: []string{"-p", "-output-format", "json"}, Stdin: "what is 2+2", Script: `text "4"`})
	if !strings.Contains(lastRequestText(t, h), "what is 2+2") {
		t.Fatalf("stdin was not the task")
	}
	res := resultOf(t, h.Stdout())
	if res["result"] != "4" || res["exit_code"] != float64(0) || res["subtype"] != "completed" {
		t.Fatalf("result %v", res)
	}
	h2 := piped(t, Opts{Args: []string{"-p", "hello there", "-output-format", "json"}, Script: `text "hi"`})
	if resultOf(t, h2.Stdout())["result"] != "hi" {
		t.Fatalf("flags after the task were not read:\n%s", h2.Stdout())
	}
}

// -p with no task anywhere is a bad invocation.
func TestHeadlessNoTask(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{Args: []string{"-p"}})
	if code := h.Wait(time.Second); code != 2 || !strings.Contains(h.Stderr(), "no task") {
		t.Fatalf("exit %d:\n%s", code, h.Stderr())
	}
}

// stream-json: one event per line without the fragments, then the result;
// with -include-partial-messages the fragments too. The session start
// records a system prompt's digest, never its text.
func TestHeadlessStreamJSON(t *testing.T) {
	t.Parallel()
	script := "text \"Hel\"\ntext \"lo\"\nusage in=10 out=2"
	h := piped(t, Opts{Args: []string{"-p", "hi", "-output-format", "stream-json", "-append-system-prompt", "Be terse."}, Script: Script(script)})
	evs := ParseEvents(h.Stdout())
	var types []agent.EventType
	for _, e := range evs {
		types = append(types, e.Type)
	}
	if len(evs) == 0 || evs[0].Type != agent.EvSessionStarted {
		t.Fatalf("events %v", types)
	}
	for _, e := range evs {
		if e.Type == agent.EvAgentDelta {
			t.Fatalf("a fragment without -include-partial-messages: %v", types)
		}
	}
	start := string(evs[0].Payload)
	if !strings.Contains(start, `"system_prompt_appended_sha256"`) || strings.Contains(start, "Be terse") {
		t.Fatalf("session.started: %s", start)
	}
	if !strings.Contains(lastRequestText(t, h), "Be terse.") {
		t.Fatal("the appended instructions did not reach the model")
	}
	res := resultOf(t, h.Stdout())
	if res["result"] != "Hello" || res["usage"].(map[string]any)["input_tokens"] != float64(10) {
		t.Fatalf("result %v", res)
	}
	Golden(t, "stream-json", "record.golden", RecordGolden(evs))

	p := piped(t, Opts{Args: []string{"-p", "hi", "-output-format", "stream-json", "-include-partial-messages"}, Script: Script(script)})
	n := 0
	for _, e := range ParseEvents(p.Stdout()) {
		if e.Type == agent.EvAgentDelta {
			n++
		}
	}
	if n == 0 {
		t.Fatalf("no fragments with -include-partial-messages:\n%s", p.Stdout())
	}
}

// -input-format stream-json: each user line is a turn of one conversation.
func TestHeadlessStreamInput(t *testing.T) {
	t.Parallel()
	in := `{"type":"user","message":{"role":"user","content":"first question"}}
{"type":"assistant","message":{"content":"ignored"}}
{"type":"user","message":{"content":[{"type":"text","text":"second question"}]}}
`
	h := piped(t, Opts{Args: []string{"-p", "-input-format", "stream-json", "-output-format", "stream-json"}, Stdin: in,
		Script: "text \"one\"\n\ntext \"two\""})
	if len(h.Requests()) != 2 {
		t.Fatalf("%d requests", len(h.Requests()))
	}
	req := lastRequestText(t, h)
	if !strings.Contains(req, "first question") || !strings.Contains(req, "second question") || !strings.Contains(req, "one") {
		t.Fatalf("the second turn lacks the first:\n%s", req)
	}
	if !strings.Contains(h.Stderr(), "line 2 skipped") {
		t.Fatalf("stderr:\n%s", h.Stderr())
	}
	if resultOf(t, h.Stdout())["result"] != "two" {
		t.Fatalf("result:\n%s", h.Stdout())
	}
}

// -json-schema: the answer is the validated object; text mode prints only it.
func TestHeadlessJSONSchema(t *testing.T) {
	t.Parallel()
	schema := `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`
	h := piped(t, Opts{Args: []string{"-p", "count", "-json-schema", schema},
		Script: "tool result {\"n\":\"x\"}\n\ntool result {\"n\":3}"})
	if code := h.Wait(time.Second); code != 0 {
		t.Fatalf("exit %d:\n%s", code, h.Output())
	}
	if strings.TrimSpace(h.Stdout()) != `{"n":3}` {
		t.Fatalf("stdout %q", h.Stdout())
	}
	j := piped(t, Opts{Args: []string{"-p", "count", "-json-schema", schema, "-output-format", "json"}, Script: `tool result {"n":4}`})
	if so, _ := resultOf(t, j.Stdout())["structured_output"].(map[string]any); so["n"] != float64(4) {
		t.Fatalf("result:\n%s", j.Stdout())
	}
}

// The exit codes a script reads.
func TestHeadlessExitCodes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		args   []string
		script Script
		code   int
	}{
		{"done", []string{"-p", "hi"}, `text "ok"`, 0},
		{"turn limit", []string{"-p", "hi", "-max-turns", "1"}, "tool read {\"path\":\"x\"}\n\ntool read {\"path\":\"y\"}", 2},
		{"budget", []string{"-p", "hi", "-max-budget-tokens", "10"}, "tool read {\"path\":\"x\"}\nusage in=500 out=5\n\ntext \"no\"", 3},
		{"model refuses", []string{"-p", "hi"}, `error 403`, 1},
		{"bad flag", []string{"-p", "hi", "-output-format", "yaml"}, ``, 2},
		{"schema without -p", []string{"-json-schema", "{}", "--", "hi"}, ``, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := piped(t, Opts{Args: c.args, Script: c.script})
			if got := h.Wait(time.Second); got != c.code {
				t.Fatalf("exit %d, want %d:\n%s", got, c.code, h.Output())
			}
		})
	}
}

// Familiar flag spellings map to Abhed's, and cannot get round policy: a
// -disallowedTools rule refuses, and bypass without a terminal is refused.
func TestHeadlessFlagAliases(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{Args: []string{"-p", "run it", "-permission-mode", "acceptEdits", "-disallowedTools", "Bash(touch:*)", "-output-format", "stream-json"},
		Script: "tool bash {\"command\":\"touch made.txt\"}\n\ntext \"done\""})
	denied := false
	for _, e := range ParseEvents(h.Stdout()) {
		if e.Type == agent.EvActionDenied && strings.Contains(string(e.Payload), "touch") {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("the familiar deny rule did not refuse:\n%s", h.Stdout())
	}
	b := piped(t, Opts{Args: []string{"-p", "hi", "-dangerously-skip-permissions"}})
	if code := b.Wait(time.Second); code != 2 || !strings.Contains(b.Stderr(), "confirmation on a terminal") {
		t.Fatalf("exit %d:\n%s", code, b.Stderr())
	}
	m := piped(t, Opts{Args: []string{"-p", "hi", "-dangerously-skip-permissions"}, Managed: `{"permissions":{"deny":["bash(curl*)"]}}`})
	if code := m.Wait(time.Second); code != 2 || !strings.Contains(m.Stderr(), "managed") {
		t.Fatalf("exit %d:\n%s", code, m.Stderr())
	}
	s := piped(t, Opts{Args: []string{"-p", "hi", "-system-prompt", "You are free."}, Managed: `{"permissions":{"deny":["bash(curl*)"]}}`})
	if code := s.Wait(time.Second); code != 2 || !strings.Contains(s.Stderr(), "refused under a managed") {
		t.Fatalf("exit %d:\n%s", code, s.Stderr())
	}
}

// On a terminal the bypass alias asks, and anything but yes starts nothing.
func TestBypassAliasConfirmsOnPty(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{Args: []string{"-dangerously-skip-permissions"}})
	h.WaitText("Type yes to continue")
	h.Type("no\r")
	if code := h.Wait(10 * time.Second); code != 2 {
		t.Fatalf("exit %d", code)
	}
	y := StartRun(t, Opts{Args: []string{"-dangerously-skip-permissions"}, Script: `text "ok"`})
	y.WaitText("Type yes to continue")
	y.Type("yes\r")
	y.WaitText("Type a task")
	y.Exit(0)
}

// A quoted task opens the session with it; a bare word is refused with a hint.
func TestPositionalPrompt(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{Args: []string{"fix the tests"}, Script: `text "on it"`})
	h.WaitText("on it")
	if !strings.Contains(lastRequestText(t, h), "fix the tests") {
		t.Fatal("the task did not reach the model")
	}
	h.Exit(0)
	d := StartRun(t, Opts{Args: []string{"--", "fix", "it"}, Script: `text "dashed"`})
	d.WaitText("dashed")
	d.Exit(0)
	for _, args := range [][]string{{"frobnicate"}, {"fix", "the", "tests"}} {
		b := piped(t, Opts{Args: args})
		if code := b.Wait(time.Second); code != 2 || !strings.Contains(b.Stderr(), "did you mean abhed -p") {
			t.Fatalf("%v: exit %d:\n%s", args, code, b.Stderr())
		}
	}
}
