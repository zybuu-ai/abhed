package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// secondModel is a scripted endpoint beside the CLI's own, answering with
// reply to its nth request and keeping each request body.
type secondModel struct {
	url    string
	mu     sync.Mutex
	bodies []string
}

func newSecondModel(t *testing.T, reply func(w io.Writer, n int)) *secondModel {
	t.Helper()
	m := &secondModel{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.bodies = append(m.bodies, string(body))
		n := len(m.bodies)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		reply(w, n)
	}))
	t.Cleanup(srv.Close)
	m.url = srv.URL
	return m
}

func (m *secondModel) requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.bodies...)
}

func textReply(text string) func(w io.Writer, n int) {
	return func(w io.Writer, _ int) {
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", text)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}
}

// startTwoModels is an interactive CLI on provider "a" (the session's own
// scripted model) with provider "b" configured beside it.
func startTwoModels(t *testing.T, b *secondModel) *cliSession {
	t.Helper()
	return startCLIConfig(t, func(w io.Writer, _ int, _ string) { textReply("from a")(w, 0) }, func(url string) string {
		return `{"model":{"default":"a","providers":{` +
			`"a":{"type":"openai-compatible","base_url":"` + url + `","model":"model-a","context_window":8192},` +
			`"b":{"type":"openai-compatible","base_url":"` + b.url + `","model":"model-b","context_window":16384}}}}`
	})
}

// run sends a prompt and waits for its task to finish, whichever model answers.
func (c *cliSession) run(prompt string) {
	c.t.Helper()
	c.tasks++
	fmt.Fprintln(c.stdin, prompt)
	c.waitFor(func(out string) bool { return strings.Count(out, " in / ") >= c.tasks }, "the task to finish")
}

func (c *cliSession) requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

// systemOf is a request's system prompt.
func systemOf(t *testing.T, body string) string {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil || len(req.Messages) == 0 || req.Messages[0].Role != "system" {
		t.Fatalf("no system prompt in the request: %v", err)
	}
	return req.Messages[0].Content
}

func callModels(events []agent.Event) string {
	var out []string
	for _, ev := range events {
		if ev.Type == agent.EvModelCall {
			var mc agent.ModelCall
			_ = json.Unmarshal(ev.Payload, &mc)
			out = append(out, mc.Model)
		}
	}
	return strings.Join(out, ",")
}

// /model mid-conversation: the next turn goes to the new model, which the
// prompt names, the conversation is kept, and the record says so.
func TestCLIModelSwitchAnswersTheNextTurn(t *testing.T) {
	b := newSecondModel(t, textReply("from b"))
	c := startTwoModels(t, b)
	c.run("Remember the codeword ZEBRA-41.")
	c.command("/model b", "switched to model-b")
	c.run("What is the codeword?")

	got := b.requests()
	if c.requests() != 1 || len(got) != 1 {
		t.Fatalf("model-a answered %d requests and model-b %d; want 1 each", c.requests(), len(got))
	}
	if sys := systemOf(t, got[0]); !strings.Contains(sys, "Model: model-b") || strings.Contains(sys, "model-a") {
		t.Fatalf("the prompt sent to model-b does not name it:\n%s", sys)
	}
	if !strings.Contains(conversationOf(t, got[0]), "ZEBRA-41") {
		t.Fatal("the switch lost the conversation")
	}
	events := c.export()
	if m := callModels(events); m != "model-a,model-b" {
		t.Fatalf("the record names the calls' models as %q", m)
	}
	if agent.ProviderOf(events) != "b" {
		t.Fatal("the switch is not in the record")
	}
	c.command("/model", "current: model-b (b)")
}

// A conversation started after /model, and after /clear, runs on the model
// chosen, and so do the subagents it spawns.
func TestCLISwitchedModelCarriesToNewSessionsAndSubagents(t *testing.T) {
	task := `{"prompt":"look","description":"look around","agent_type":"explore"}`
	b := newSecondModel(t, func(w io.Writer, n int) {
		if n == 1 {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"task","arguments":`+fmt.Sprintf("%q", task)+`}}]}}]}`)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		textReply("from b")(w, n)
	})
	c := startTwoModels(t, b)
	c.command("/model b", "switched to model-b")
	c.run("Delegate a look around.")
	c.command("/clear", "context cleared")
	c.run("Hello again.")

	got := b.requests()
	if c.requests() != 0 {
		t.Fatalf("model-a answered %d requests after the switch to model-b", c.requests())
	}
	// The parent's two turns, the subagent's one, and the new session's one.
	if len(got) != 4 {
		t.Fatalf("model-b answered %d requests; want 4", len(got))
	}
	for i, body := range got {
		if sys := systemOf(t, body); !strings.Contains(sys, "Model: model-b") {
			t.Fatalf("request %d's prompt does not name model-b:\n%s", i+1, sys)
		}
	}
}

// /resume continues a session on the model the CLI uses now, says it last ran
// on another, and records the move so a later resume continues where it ran.
func TestCLIResumeOnAnotherModelSaysSoAndRecordsIt(t *testing.T) {
	b := newSecondModel(t, textReply("from b"))
	c := startTwoModels(t, b)
	c.run("Remember the codeword ZEBRA-41.")
	first := checkRecord(t, c.export(), 1)
	c.command("/clear", "context cleared")
	c.command("/model b", "switched to model-b")
	c.command("/resume "+first, "it last ran on model-a and continues on model-b")
	c.run("What is the codeword?")

	got := b.requests()
	if c.requests() != 1 || len(got) != 1 || !strings.Contains(conversationOf(t, got[0]), "ZEBRA-41") {
		t.Fatalf("the resumed conversation did not continue on model-b: model-a %d, model-b %d", c.requests(), len(got))
	}
	events := c.export()
	if agent.ProviderOf(events) != "b" || callModels(events) != "model-a,model-b" {
		t.Fatalf("the record does not say the session moved to model-b: provider %q, calls %q",
			agent.ProviderOf(events), callModels(events))
	}
}

// A move the record refuses is kept, so the task after the refused one records
// it before its message rather than running on the new model unrecorded.
func TestCLIRefusedMoveIsRecordedByTheNextTask(t *testing.T) {
	st := agent.NewMemStore()
	rec := agent.NewRecorder(st, "s1", "")
	refuse := true
	rec.Gate = func() error {
		if refuse {
			refuse = false
			return fmt.Errorf("the store refused the write")
		}
		return nil
	}
	moved := &agent.ModelSwitched{Provider: "b", Model: "model-b", From: "model-a"}
	cs := &cliState{loop: agent.NewLoop(nil, nil, nil, nil, nil, rec, agent.DefaultConfig()), moved: moved}
	if err := recordMove(cs); err == nil {
		t.Fatal("a refused write was not reported, so the task would run")
	}
	if cs.moved == nil {
		t.Fatal("a refused move was dropped; the next task would run on model-b unrecorded")
	}
	if err := recordMove(cs); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "next"}); err != nil {
		t.Fatal(err)
	}
	events, _ := st.Events("s1")
	if len(events) != 2 || events[0].Type != agent.EvModelSwitched || events[1].Type != agent.EvUserMessage || cs.moved != nil {
		t.Fatalf("the move is not recorded once, before the next message: %+v", events)
	}
}
