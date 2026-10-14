package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/secrets"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// globModel answers every request with one glob call, after hold returns, and
// keeps each request body.
type globModel struct {
	mu     sync.Mutex
	bodies []string
	hold   func(n int)
	// text answers with a closing message instead of a call.
	text bool
}

func (m *globModel) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.bodies = append(m.bodies, string(b))
		n := len(m.bodies)
		m.mu.Unlock()
		if m.hold != nil {
			m.hold(n)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if m.text {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`)
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c`+fmt.Sprint(n)+`","function":{"name":"glob","arguments":"{\"pattern\":\"*\"}"}}]}}]}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (m *globModel) requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.bodies...)
}

// rpcWorkspace writes a trusted workspace configuration on the stub model.
func rpcWorkspace(t *testing.T, url, extra string) string {
	t.Helper()
	managedConfig(t, "")
	ws := t.TempDir()
	cfg := `{"model": {"default": "fake", "providers": {"fake": {"type": "openai-compatible",
		"base_url": "` + url + `", "model": "m"}}}` + extra + `}`
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.TrustEnv, "1") // the test wrote this configuration
	return ws
}

// rpcPipe runs abhed rpc on pipes the test writes to and reads from.
type rpcPipe struct {
	in    *os.File
	lines chan string
	done  chan struct{}
}

func startRPC(t *testing.T, ws string) *rpcPipe {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	s := &rpcPipe{in: inW, lines: make(chan string, 4096), done: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for sc.Scan() {
			s.lines <- sc.Text()
		}
		close(s.lines)
	}()
	go func() {
		defer close(s.done)
		rpcCmd(ws, "")
		_ = outW.Close()
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case <-s.done:
		case <-time.After(20 * time.Second):
			t.Error("rpc did not end")
		}
		os.Stdin, os.Stdout = oldIn, oldOut
	})
	return s
}

func (s *rpcPipe) send(line string) { fmt.Fprintln(s.in, line) }

// waitFor reads output until a line holds want, returning everything read.
func (s *rpcPipe) waitFor(t *testing.T, want string) string {
	t.Helper()
	var got strings.Builder
	deadline := time.After(20 * time.Second)
	for {
		select {
		case l, ok := <-s.lines:
			if !ok {
				t.Fatalf("rpc ended before %s:\n%s", want, got.String())
			}
			got.WriteString(l + "\n")
			if strings.Contains(l, want) {
				return got.String()
			}
		case <-deadline:
			t.Fatalf("no %s within the deadline:\n%s", want, got.String())
		}
	}
}

// rpc binds the trusted workspace's limits.max_turns, as the terminal does:
// it once ran to the default hundred.
func TestRPCHonoursTheConfiguredTurnLimit(t *testing.T) {
	m := &globModel{}
	ws := rpcWorkspace(t, m.serve(t).URL, `, "limits": {"max_turns": 2}`)
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start"}`)
	s.send(`{"id":"2","method":"prompt","prompt":"look around"}`)
	out := s.waitFor(t, `"id":"2"`)
	if !strings.Contains(out, "ended as max_turns") {
		t.Errorf("the run did not end at the turn limit:\n%s", out)
	}
	if n := len(m.requests()); n != 2 {
		t.Errorf("the model was called %d times, want 2", n)
	}
}

// A steer sent while a prompt runs reaches that run; it was once read only
// after the run ended.
func TestRPCSteerReachesTheRunningPrompt(t *testing.T) {
	steered, inFirst := make(chan struct{}), make(chan struct{})
	m := &globModel{}
	// The first turn waits for the steer to be acknowledged.
	m.hold = func(n int) {
		if n == 1 {
			close(inFirst)
			select {
			case <-steered:
			case <-time.After(5 * time.Second):
			}
		}
	}
	ws := rpcWorkspace(t, m.serve(t).URL, `, "limits": {"max_turns": 3}`)
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start"}`)
	s.waitFor(t, `"type":"ready"`)
	s.send(`{"id":"2","method":"prompt","prompt":"look around"}`)
	s.send(`{"id":"3","method":"prompt","prompt":"SECOND-PROMPT"}`)
	<-inFirst
	s.send(`{"id":"4","method":"steer","prompt":"STEER-TEXT"}`)
	out := s.waitFor(t, `"type":"steered"`)
	close(steered)
	if strings.Contains(out, `"id":"2"`) {
		t.Errorf("steer was answered only after the prompt:\n%s", out)
	}
	s.waitFor(t, `"id":"2"`)
	reqs := m.requests()
	if len(reqs) < 2 || !strings.Contains(reqs[1], "STEER-TEXT") || strings.Contains(reqs[1], "SECOND-PROMPT") {
		t.Fatalf("the running prompt's next turn did not carry the steer alone: %d requests", len(reqs))
	}
	// The second prompt waited its turn rather than being dropped.
	if out := s.waitFor(t, `"id":"3"`); !strings.Contains(out, "SECOND-PROMPT") {
		t.Errorf("the queued prompt never ran:\n%s", out)
	}
}

// acp builds its sessions with the configured turn limit, as rpc does.
func TestACPSessionTakesTheConfiguredTurnLimit(t *testing.T) {
	var made *scriptedACPAgent
	newACPAgent = func(_ context.Context, opts abhed.Options) (acpAgent, error) {
		made = &scriptedACPAgent{opts: opts}
		return made, nil
	}
	defer func() {
		newACPAgent = func(ctx context.Context, o abhed.Options) (acpAgent, error) { return abhed.New(ctx, o) }
	}()
	cl := newACPClient(t, func(string, json.RawMessage) any { return nil })
	cl.request(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	cl.request(2, "session/new", map[string]any{"cwd": "/ws", "mcpServers": []any{}})
	if made == nil || !made.opts.ConfiguredLimits {
		t.Fatal("the acp session does not take the configuration's turn limit")
	}
}

// holdFirst makes the model's first request wait for release, and closes
// entered when it arrives.
func holdFirst(m *globModel) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	m.hold = func(n int) {
		if n == 1 {
			close(entered)
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
		}
	}
	return entered, release
}

// A steer sent straight after start reaches the session that start makes,
// rather than finding no session yet.
func TestRPCSteerRightAfterStartIsKept(t *testing.T) {
	m := &globModel{text: true}
	ws := rpcWorkspace(t, m.serve(t).URL, "")
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start"}`)
	s.send(`{"id":"2","method":"steer","prompt":"EARLY-STEER"}`)
	if out := s.waitFor(t, `"id":"2"`); !strings.Contains(out, `"type":"queued"`) {
		t.Fatalf("the steer was not kept for the new session:\n%s", out)
	}
	s.send(`{"id":"3","method":"prompt","prompt":"go"}`)
	s.waitFor(t, `"id":"3"`)
	if reqs := m.requests(); len(reqs) == 0 || !strings.Contains(reqs[0], "EARLY-STEER") {
		t.Fatal("the held steer never reached the model")
	}
}

// A steer without text in "prompt", such as one sent with "text", is refused
// naming the field, not answered steered with nothing in it.
func TestRPCSteerWithoutPromptIsRefused(t *testing.T) {
	m := &globModel{text: true}
	ws := rpcWorkspace(t, m.serve(t).URL, "")
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start"}`)
	s.waitFor(t, `"id":"1"`)
	for i, line := range []string{`{"id":"2","method":"steer","text":"Also count lines."}`, `{"id":"3","method":"steer","prompt":"  "}`} {
		s.send(line)
		id := fmt.Sprintf(`"id":"%d"`, i+2)
		if out := s.waitFor(t, id); !strings.Contains(out, `"type":"error"`) || !strings.Contains(out, `\"prompt\"`) {
			t.Fatalf("%s: want an error naming the prompt field:\n%s", line, out)
		}
	}
	s.send(`{"id":"4","method":"prompt","prompt":"go"}`)
	s.waitFor(t, `"id":"4"`)
	if reqs := m.requests(); len(reqs) == 0 || strings.Contains(reqs[0], "count lines") {
		t.Fatal("the refused steer reached the model")
	}
}

// A steer read after a second start, still queued behind a prompt, goes to
// that new session; it once went to the old one, which the start then closed.
func TestRPCSteerAfterAQueuedStartGoesToTheNewSession(t *testing.T) {
	m := &globModel{text: true}
	entered, release := holdFirst(m)
	ws := rpcWorkspace(t, m.serve(t).URL, "")
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start"}`)
	s.waitFor(t, `"type":"ready"`)
	s.send(`{"id":"2","method":"prompt","prompt":"first"}`)
	<-entered
	s.send(`{"id":"3","method":"start"}`)
	s.send(`{"id":"4","method":"steer","prompt":"FOR-THE-NEW-SESSION"}`)
	if out := s.waitFor(t, `"id":"4"`); !strings.Contains(out, `"type":"queued"`) {
		t.Fatalf("the steer was not held for the new session:\n%s", out)
	}
	close(release)
	s.waitFor(t, `"id":"3"`)
	s.send(`{"id":"5","method":"prompt","prompt":"second"}`)
	s.waitFor(t, `"id":"5"`)
	reqs := m.requests()
	if strings.Contains(reqs[0], "FOR-THE-NEW-SESSION") || !strings.Contains(reqs[len(reqs)-1], "FOR-THE-NEW-SESSION") {
		t.Fatalf("the steer did not reach the new session's prompt (%d requests)", len(reqs))
	}
}

// A steer answered steered is read by the model even when the run was about
// to end, before the prompt's answer; one only queued is named if never read.
func TestRPCSteeredIsDeliveredAndQueuedIsAccountedFor(t *testing.T) {
	m := &globModel{text: true}
	entered, release := holdFirst(m)
	ws := rpcWorkspace(t, m.serve(t).URL, "")
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start"}`)
	s.waitFor(t, `"type":"ready"`)
	s.send(`{"id":"2","method":"prompt","prompt":"first"}`)
	<-entered
	s.send(`{"id":"3","method":"steer","prompt":"LATE-STEER"}`)
	s.waitFor(t, `"type":"steered"`)
	close(release)
	s.waitFor(t, `"id":"2"`)
	reqs := m.requests()
	if len(reqs) < 2 || !strings.Contains(reqs[len(reqs)-1], "LATE-STEER") {
		t.Fatalf("a steer answered steered was not delivered before the answer (%d requests)", len(reqs))
	}

	s.send(`{"id":"4","method":"steer","prompt":"NEVER-READ"}`)
	if out := s.waitFor(t, `"id":"4"`); !strings.Contains(out, `"type":"queued"`) {
		t.Fatalf("an idle steer should be answered queued:\n%s", out)
	}
	s.send(`{"id":"5","method":"quit"}`)
	if out := s.waitFor(t, `"type":"bye"`); !strings.Contains(out, "1 queued steer message(s) were not delivered") {
		t.Fatalf("quit did not say the queued steer was never read:\n%s", out)
	}
}

// Requests beyond the cap are refused by name, not held without bound.
func TestRPCRefusesRequestsBeyondTheCap(t *testing.T) {
	m := &globModel{text: true}
	entered, release := holdFirst(m)
	defer close(release)
	ws := rpcWorkspace(t, m.serve(t).URL, "")
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start"}`)
	s.waitFor(t, `"type":"ready"`)
	s.send(`{"id":"2","method":"prompt","prompt":"first"}`)
	<-entered
	for i := range rpcMaxPending + 1 {
		s.send(fmt.Sprintf(`{"id":"u%d","method":"providers"}`, i))
	}
	if out := s.waitFor(t, `"id":"u256"`); !strings.Contains(out, "requests are already waiting") {
		t.Fatalf("the request past the cap was not refused:\n%s", out)
	}
}

// A stored secret's value in a prompt or a steer reaches the model as
// [secret:NAME]; the user.message event names it for the caller to say so.
func TestRPCWithholdsATypedSecret(t *testing.T) {
	steered, inFirst := make(chan struct{}), make(chan struct{})
	m := &globModel{}
	m.hold = func(n int) {
		if n == 1 {
			close(inFirst)
			select {
			case <-steered:
			case <-time.After(5 * time.Second):
			}
		}
	}
	ws := rpcWorkspace(t, m.serve(t).URL, `, "limits": {"max_turns": 2}`)
	// After rpcWorkspace, which gives the test a home of its own.
	t.Setenv(secrets.EnvFile, "")
	for name, value := range map[string]string{"API_TOKEN": "tok-9f8e7d-rpc", "DB_PASS": "hunter2-hunter2-rpc"} {
		if err := secrets.Default().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start"}`)
	s.waitFor(t, `"type":"ready"`)
	s.send(`{"id":"2","method":"prompt","prompt":"use tok-9f8e7d-rpc"}`)
	<-inFirst
	s.send(`{"id":"3","method":"steer","prompt":"and hunter2-hunter2-rpc"}`)
	out := s.waitFor(t, `"type":"steered"`)
	close(steered)
	out += s.waitFor(t, `"id":"2"`)
	reqs := m.requests()
	if len(reqs) < 2 {
		t.Fatalf("%d model requests", len(reqs))
	}
	for _, r := range reqs {
		if strings.Contains(r, "tok-9f8e7d-rpc") || strings.Contains(r, "hunter2-hunter2-rpc") {
			t.Fatalf("the model was sent a stored value: %s", r)
		}
	}
	if !strings.Contains(reqs[1], "use [secret:API_TOKEN]") || !strings.Contains(reqs[1], "and [secret:DB_PASS]") {
		t.Fatalf("the model was not sent the placeholders: %s", reqs[1])
	}
	for _, want := range []string{`"secrets_withheld":"API_TOKEN"`, `"secrets_withheld":"DB_PASS"`} {
		if !strings.Contains(out, want) {
			t.Errorf("no event with %s:\n%s", want, out)
		}
	}
}

// A prompt's run goes on for a steer queued as it ended when it ended
// cleanly or a hook refused its own message, as the terminal's does.
func TestRPCRunsOnForAQueuedSteer(t *testing.T) {
	for _, c := range []struct {
		name   string
		err    error
		queued int
		want   bool
	}{
		{"completed", nil, 1, true},
		{"nothing queued", nil, 0, false},
		{"message refused by a hook", &abhed.EndedError{Reason: agent.TermPromptRefused}, 1, true},
		{"out of turns", &abhed.EndedError{Reason: agent.TermMaxTurns}, 1, false},
		{"interrupted", &abhed.EndedError{Reason: agent.TermUserInterrupt}, 1, false},
		{"failed", errors.New("model down"), 1, false},
	} {
		if got := rpcRunsOn(c.err, c.queued); got != c.want {
			t.Errorf("%s: runs on %v, want %v", c.name, got, c.want)
		}
	}
}

// A user_prompt_submit hook refuses an rpc prompt: the model is never sent
// it, and the answer says the run ended refused.
func TestRPCPromptHookRefusesAPrompt(t *testing.T) {
	m := &globModel{text: true}
	ws := rpcWorkspace(t, m.serve(t).URL, `, "extensions":[{"name":"gate","command":"bash","args":[`+
		jsonQuote(writeScript(t, gatekeeper))+`],"events":["user_prompt_submit"]}]`)
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start"}`)
	s.waitFor(t, `"type":"ready"`)
	s.send(`{"id":"2","method":"prompt","prompt":"the forbidden thing"}`)
	if out := s.waitFor(t, `"id":"2"`); !strings.Contains(out, "ended as prompt_refused") {
		t.Fatalf("the refused prompt's answer:\n%s", out)
	}
	if n := len(m.requests()); n != 0 {
		t.Fatalf("the model was sent %d requests; the refused prompt must not be one", n)
	}
}

// A -p run's prompt a user_prompt_submit hook refuses is never sent, and the
// person is told why.
func TestHeadlessPromptHookRefusesThePrompt(t *testing.T) {
	g := newSessRig(t)
	cfg := `{"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + g.url +
		`","model":"m","context_window":8192}}},"extensions":[{"name":"gate","command":"bash","args":[` +
		jsonQuote(writeScript(t, gatekeeper)) + `],"events":["user_prompt_submit"]}]}`
	if err := os.WriteFile(filepath.Join(g.ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := g.cmd("-p", "the forbidden thing").CombinedOutput()
	g.mu.Lock()
	n := len(g.bodies)
	g.mu.Unlock()
	if n != 0 || !strings.Contains(string(out), "your message was not sent: that topic is off limits") {
		t.Fatalf("%d model requests; output:\n%s", n, out)
	}
}

// A steer that arrives while the hook is judging the prompt is answered, and
// the prompt's own refusal is not lost: the reply is the refusal error,
// carrying the steer's answer.
func TestRPCKeepsTheRefusalWhenASteerArrivesDuringIt(t *testing.T) {
	m := &globModel{text: true}
	slowGate := `#!/bin/bash
while IFS= read -r line; do
  case "$line" in
    *'"event":"user_prompt_submit"'*forbidden*) sleep 1; echo '{"block":true,"reason":"that topic is off limits"}' ;;
    *) echo '{}' ;;
  esac
done
`
	ws := rpcWorkspace(t, m.serve(t).URL, `, "extensions":[{"name":"gate","command":"bash","args":[`+
		jsonQuote(writeScript(t, slowGate))+`],"events":["user_prompt_submit"]}]`)
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start"}`)
	s.waitFor(t, `"type":"ready"`)
	s.send(`{"id":"2","method":"prompt","prompt":"the forbidden thing"}`)
	time.Sleep(300 * time.Millisecond) // the hook is judging the prompt
	s.send(`{"id":"3","method":"steer","prompt":"a fair question"}`)
	out := s.waitFor(t, `"id":"2"`)
	if !strings.Contains(out, `"type":"steered"`) {
		t.Skipf("the steer did not arrive while the prompt ran, so this checks nothing:\n%s", out)
	}
	last := out[strings.LastIndex(out, `{"id":"2"`):]
	if !strings.Contains(last, `"type":"error"`) || !strings.Contains(last, "prompt_refused") || !strings.Contains(last, `"answer":"done"`) {
		t.Fatalf("the prompt's reply: %s", last)
	}
}
