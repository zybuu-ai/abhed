package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// TestConversationHelper is the interactive CLI the conversation tests drive
// over a pipe, in the workspace they name.
func TestConversationHelper(t *testing.T) {
	ws := os.Getenv("ABHED_CONV_WS")
	if ws == "" {
		t.Skip("run by the conversation tests")
	}
	if home := os.Getenv("ABHED_CONV_HOME"); home != "" {
		t.Setenv("HOME", home) // TestMain gave this process a home of its own
	}
	os.Exit(Main([]string{"-C", ws}))
}

// syncBuffer is output written by one goroutine and read by the test.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// cliSession is an interactive abhed on a pipe, talking to a model that
// answers every request with text and keeps each request body.
type cliSession struct {
	t      *testing.T
	ws     string
	stdin  io.WriteCloser
	out    *syncBuffer
	cmd    *exec.Cmd
	mu     sync.Mutex
	bodies []string
	tasks  int
}

func startCLI(t *testing.T) *cliSession {
	t.Helper()
	return startCLIWith(t, func(w io.Writer, n int, _ string) {
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"noted %d\"}}]}\n\n", n)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	})
}

// startCLIWith is startCLI with a model that streams reply to the nth request.
func startCLIWith(t *testing.T, reply func(w io.Writer, n int, body string)) *cliSession {
	t.Helper()
	return startCLIConfig(t, reply, func(url string) string {
		return `{"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + url + `","model":"m","context_window":8192}}}}`
	})
}

// startCLIConfig is startCLIWith under the configuration config makes from the model's URL.
func startCLIConfig(t *testing.T, reply func(w io.Writer, n int, body string), config func(url string) string) *cliSession {
	t.Helper()
	return startCLIPrepared(t, reply, config, nil)
}

// startCLIEnv is startCLIConfig with env added to the helper's environment, where it wins.
func startCLIEnv(t *testing.T, reply func(w io.Writer, n int, body string), config func(url string) string, env ...string) *cliSession {
	t.Helper()
	return startCLIPrepared(t, reply, config, nil, env...)
}

// startCLIPrepared is startCLIConfig with prep run on the workspace before
// the process starts, for files it reads at start-up, and env added to the
// helper's environment, where it wins.
func startCLIPrepared(t *testing.T, reply func(w io.Writer, n int, body string), config func(url string) string, prep func(ws, home string), env ...string) *cliSession {
	t.Helper()
	c := &cliSession{t: t, out: &syncBuffer{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, string(body))
		n := len(c.bodies)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		reply(w, n, string(body))
	}))
	t.Cleanup(srv.Close)

	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	c.ws = ws
	cfg := config(srv.URL)
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if prep != nil {
		prep(ws, home)
	}
	c.cmd = exec.Command(os.Args[0], "-test.run=^TestConversationHelper$")
	c.cmd.Env = append(os.Environ(), "ABHED_CONV_WS="+ws, "HOME="+home, "ABHED_CONV_KEEP_HOME=1", "USERPROFILE="+t.TempDir(),
		"ABHED_TRUST_WORKSPACE=1") // the test wrote this configuration
	c.cmd.Env = append(c.cmd.Env, env...)
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	c.stdin = stdin
	c.cmd.Stdout, c.cmd.Stderr = c.out, c.out
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.stdin.Close()
		done := make(chan struct{})
		go func() { _ = c.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = c.cmd.Process.Kill() // the helper this test started
			<-done
		}
	})
	return c
}

// task sends a prompt, waits for its turn to finish, and returns the
// conversation the model was sent.
func (c *cliSession) task(prompt string) string {
	c.t.Helper()
	c.tasks++
	fmt.Fprintln(c.stdin, prompt)
	c.waitFor(func(out string) bool { return strings.Count(out, " in / ") >= c.tasks }, "the task to finish")
	c.mu.Lock()
	body := c.bodies[len(c.bodies)-1]
	c.mu.Unlock()
	return conversationOf(c.t, body)
}

// conversationOf is a request's messages after the system prompt, as JSON.
func conversationOf(t *testing.T, body string) string {
	t.Helper()
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil || len(req.Messages) == 0 {
		t.Fatalf("not a chat request: %v", err)
	}
	out, _ := json.Marshal(req.Messages[1:])
	return string(out)
}

// command sends a slash command and waits for text it prints.
func (c *cliSession) command(line, want string) {
	c.t.Helper()
	before := strings.Count(c.out.String(), want)
	fmt.Fprintln(c.stdin, line)
	c.waitFor(func(out string) bool { return strings.Count(out, want) > before }, want)
}

// declineNumber is the number of the last approval's No, its last answer,
// read from the "answer 1-N:" line the prompt prints.
func (c *cliSession) declineNumber() string {
	c.t.Helper()
	out := c.out.String()
	i := strings.LastIndex(out, "answer 1-")
	if i < 0 || i+len("answer 1-") >= len(out) {
		c.t.Fatalf("no numbered approval:\n%s", out)
	}
	return out[i+len("answer 1-") : i+len("answer 1-")+1]
}

func (c *cliSession) waitFor(ok func(string) bool, what string) {
	c.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !ok(c.out.String()); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			c.t.Fatalf("waited for %s:\n%s", what, c.out.String())
		}
	}
}

// export writes the conversation's record and returns it.
func (c *cliSession) export() []agent.Event {
	c.t.Helper()
	// Inside the workspace: an export elsewhere asks first.
	dir, err := os.MkdirTemp(c.ws, "export-")
	if err != nil {
		c.t.Fatal(err)
	}
	path := filepath.Join(dir, "record.json")
	c.command("/export "+path, "wrote ")
	raw, err := os.ReadFile(path)
	if err != nil {
		c.t.Fatal(err)
	}
	var events []agent.Event
	if err := json.Unmarshal(raw, &events); err != nil {
		c.t.Fatal(err)
	}
	return events
}

// checkRecord fails unless events are one session with a contiguous sequence
// and a session.ended for each of its tasks.
func checkRecord(t *testing.T, events []agent.Event, tasks int) string {
	t.Helper()
	ended := 0
	for i, ev := range events {
		if ev.SessionID != events[0].SessionID {
			t.Fatalf("event %d is in session %s, not %s", ev.Seq, ev.SessionID, events[0].SessionID)
		}
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i+1, ev.Seq)
		}
		if ev.Type == agent.EvSessionEnded {
			ended++
		}
	}
	if ended != tasks {
		t.Fatalf("%d session.ended for %d tasks", ended, tasks)
	}
	return events[0].SessionID
}

// endOf is the seq of the nth session.ended in events.
func endOf(t *testing.T, events []agent.Event, n int) int64 {
	t.Helper()
	for _, ev := range events {
		if ev.Type == agent.EvSessionEnded {
			if n--; n == 0 {
				return ev.Seq
			}
		}
	}
	t.Fatal("no such task end in the record")
	return 0
}

// A later task sees what was said in an earlier one, and the record is one
// session with a terminal event per task.
func TestCLIConversationCarriesAcrossTasks(t *testing.T) {
	c := startCLI(t)
	c.task("Remember the codeword ZEBRA-41.")
	if body := c.task("What is the codeword?"); !strings.Contains(body, "ZEBRA-41") || !strings.Contains(body, "noted 1") {
		t.Fatalf("the second task's request has no earlier turn:\n%s", body)
	}
	checkRecord(t, c.export(), 2)
}

// /fork N rebuilds the conversation to step N, and the next task continues
// from there, without what came after it.
func TestCLIForkCarriesIntoNextTask(t *testing.T) {
	c := startCLI(t)
	c.task("Remember the codeword ZEBRA-41.")
	c.task("Also remember OSPREY-58.")
	events := c.export()
	checkRecord(t, events, 2)
	c.command(fmt.Sprintf("/fork %d", endOf(t, events, 1)), "forked at step")
	body := c.task("What is the codeword?")
	if !strings.Contains(body, "ZEBRA-41") {
		t.Fatalf("the forked conversation did not reach the model:\n%s", body)
	}
	if strings.Contains(body, "OSPREY-58") {
		t.Fatalf("the turn after the fork point is still in the conversation:\n%s", body)
	}
	checkRecord(t, c.export(), 3)
}

// /resume makes a recorded session the conversation again: the next task
// sees it, and its events extend that session's sequence.
func TestCLIResumeContinuesRecordedConversation(t *testing.T) {
	c := startCLI(t)
	c.task("Remember the codeword ZEBRA-41.")
	first := checkRecord(t, c.export(), 1)
	c.command("/clear", "context cleared")
	if body := c.task("Hello."); strings.Contains(body, "ZEBRA-41") {
		t.Fatalf("/clear kept the conversation:\n%s", body)
	}
	c.command("/resume "+first, "resumed")
	if body := c.task("What is the codeword?"); !strings.Contains(body, "ZEBRA-41") {
		t.Fatalf("the resumed conversation did not reach the model:\n%s", body)
	}
	if id := checkRecord(t, c.export(), 2); id != first {
		t.Fatalf("the resumed task was recorded in %s, not %s", id, first)
	}
}

// lastEnd is the seq of the last session.ended in events.
func lastEnd(events []agent.Event) int64 {
	var seq int64
	for _, ev := range events {
		if ev.Type == agent.EvSessionEnded {
			seq = ev.Seq
		}
	}
	return seq
}

// A second /fork, at a step after the first, keeps the first fork's choice:
// the branch it abandoned does not come back.
func TestCLIForkTwiceKeepsTheBranchAbandoned(t *testing.T) {
	c := startCLI(t)
	c.task("Remember the codeword ZEBRA-41.")
	c.task("Also remember OSPREY-58.")
	c.command(fmt.Sprintf("/fork %d", endOf(t, c.export(), 1)), "forked at step")
	c.task("Also remember KESTREL-7.")
	c.command(fmt.Sprintf("/fork %d", lastEnd(c.export())), "forked at step")
	body := c.task("What are the codewords?")
	if !strings.Contains(body, "ZEBRA-41") || !strings.Contains(body, "KESTREL-7") || strings.Contains(body, "OSPREY-58") {
		t.Fatalf("the second fork did not rebuild the kept branch alone:\n%s", body)
	}
}

// /resume after a fork rebuilds the conversation as it stood, without the
// branch the fork abandoned.
func TestCLIResumeAfterForkKeepsTheBranchAbandoned(t *testing.T) {
	c := startCLI(t)
	c.task("Remember the codeword ZEBRA-41.")
	c.task("Also remember OSPREY-58.")
	events := c.export()
	c.command(fmt.Sprintf("/fork %d", endOf(t, events, 1)), "forked at step")
	c.task("Say ok.")
	c.command("/clear", "context cleared")
	c.command("/resume "+events[0].SessionID, "resumed")
	body := c.task("What are the codewords?")
	if !strings.Contains(body, "ZEBRA-41") || strings.Contains(body, "OSPREY-58") {
		t.Fatalf("the resumed conversation holds the abandoned branch:\n%s", body)
	}
}

// A resumed session's token totals go on from its record, as its turns do.
func TestCLIResumeCarriesTokenTotals(t *testing.T) {
	c := startCLI(t)
	c.task("Remember the codeword ZEBRA-41.")
	first := checkRecord(t, c.export(), 1)
	c.command("/clear", "context cleared")
	c.command("/resume "+first, "resumed")
	c.task("What is the codeword?")
	end, ok := agent.LastEnd(c.export())
	if !ok {
		t.Fatal("no session.ended in the record")
	}
	// Each model call reports 10 in and 2 out.
	if end.Turns != 2 || end.TokensIn != 20 || end.TokensOut != 4 {
		t.Fatalf("resumed end has turns %d, tokens %d in / %d out; want 2, 20 / 4", end.Turns, end.TokensIn, end.TokensOut)
	}
}

// Two CLIs whose first task starts in the same second record two sessions:
// a shared id merged their records, or dropped one, on Postgres.
func TestCLIsStartedTogetherGetTheirOwnSessions(t *testing.T) {
	a, b := startCLI(t), startCLI(t)
	// Start both early in a second, so a clock-derived id would match.
	for time.Now().Nanosecond() > 100_000_000 {
		time.Sleep(5 * time.Millisecond)
	}
	for _, c := range []*cliSession{a, b} {
		c.tasks++
		fmt.Fprintln(c.stdin, "Say ok.")
	}
	for _, c := range []*cliSession{a, b} {
		c.waitFor(func(out string) bool { return strings.Count(out, " in / ") >= 1 }, "the task to finish")
	}
	if ida, idb := checkRecord(t, a.export(), 1), checkRecord(t, b.export(), 1); ida == idb {
		t.Fatalf("both CLIs recorded session %s", ida)
	}
}

// An "always allow" chosen in one session is asked again after /clear, in the
// real interactive CLI with its own approver.
func TestCLIAlwaysAllowEndsWithClear(t *testing.T) {
	c := startCLIWith(t, func(w io.Writer, _ int, body string) {
		// A task's first request asks for a command; the one after its result ends the turn.
		if !strings.Contains(body, `"role":"tool"`) || strings.LastIndex(body, `"role":"user"`) > strings.LastIndex(body, `"role":"tool"`) {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"mkdir -p out/one\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"made\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	})
	// The answer is sent once the question is whole: a line sent sooner steers.
	asked := func() int { return strings.Count(c.out.String(), "answer 1-3:") }
	finished := func() int { return strings.Count(c.out.String(), " in / ") }

	fmt.Fprintln(c.stdin, "Make the folder.")
	c.waitFor(func(string) bool { return asked() == 1 }, "the approval")
	fmt.Fprintln(c.stdin, "2") // don't ask again this session
	c.waitFor(func(string) bool { return finished() == 1 }, "the first task to finish")

	c.command("/clear", "context cleared")
	fmt.Fprintln(c.stdin, "Make the folder again.")
	c.waitFor(func(string) bool { return asked() == 2 || finished() == 2 }, "the second task")
	if asked() != 2 {
		t.Fatalf("a scope from the cleared session approved the call:\n%s", c.out.String())
	}
	fmt.Fprintln(c.stdin, "3") // no
	c.waitFor(func(string) bool { return finished() == 2 }, "the second task to finish")
}
