package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// bgModelServer is an OpenAI-compatible stub. A prompt starting "go" starts
// one background task ("child"); the child answers after childDelay, or runs
// childCommand first when one is set; a background result is answered
// "noted"; anything else "done".
type bgModelServer struct {
	childDelay   time.Duration
	childCommand string
	calls        atomic.Int64
	// prompted records the prompts the parent was sent.
	prompted sync.Map
	// parentCommand is run by the parent for a prompt starting "run".
	parentCommand string
}

func (b *bgModelServer) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		n := b.calls.Add(1)
		var first string
		for _, m := range req.Messages {
			if m.Role == "user" {
				first = m.Content
				break
			}
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" {
			b.prompted.Store(last.Content, true)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		text := func(s string) {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", s)
		}
		call := func(name, args string) {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c`+strconv.FormatInt(n, 10)+
				`","type":"function","function":{"name":"`+name+`","arguments":`+strconv.Quote(args)+`}}]}}]}`)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		}
		switch {
		case first == "child" && last.Role == "user" && b.childCommand != "":
			call("bash", `{"command":`+strconv.Quote(b.childCommand)+`}`)
		case first == "child":
			select {
			case <-time.After(b.childDelay):
			case <-r.Context().Done():
				return
			}
			text("child result")
		case last.Role == "tool" && strings.HasPrefix(last.ToolCallID, "bgn_"):
			text("noted")
		case last.Role == "user" && strings.HasPrefix(last.Content, "run") && b.parentCommand != "":
			call("bash", `{"command":`+strconv.Quote(b.parentCommand)+`}`)
		case last.Role == "user" && strings.HasPrefix(last.Content, "go"):
			call("task", `{"prompt":"child","description":"child","background":true}`)
		default:
			text("done")
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// bgWorkspace writes a home config naming the stub and returns the workspace.
func bgWorkspace(t *testing.T, url, extra string) string {
	t.Helper()
	home, ws := t.TempDir(), t.TempDir()
	cfg := `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` +
		url + `","model":"m","context_window":8192}}}` + extra + `}`
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	return ws
}

// -p joins its background tasks: the run waits for the child, delivers its
// result at the boundary, and exits once, with the result in its events.
func TestPrintModeForcesJoin(t *testing.T) {
	m := &bgModelServer{childDelay: 300 * time.Millisecond}
	ws := bgWorkspace(t, m.start(t), `,"subagents":{"wake":"auto"}`)
	cmd := mainHelper([]string{"-C", ws, "-output-format", "json", "-p", "go"})
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("-p: %v\n%s\n%s", err, out.String(), errOut.String())
	}
	var notice, finalEnd bool
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "subagent.notice":
			notice = strings.Contains(string(ev.Payload), `"delivery":"boundary"`)
		case "session.ended":
			finalEnd = !strings.Contains(string(ev.Payload), `"background"`)
		}
	}
	if !notice || !finalEnd {
		t.Fatalf("-p did not join its child (notice %v, final end %v):\n%s", notice, finalEnd, out.String())
	}
	if !strings.Contains(errOut.String(), "runs background tasks joined") {
		t.Fatalf("no note that wake auto is ignored in -p:\n%s", errOut.String())
	}
}

// Piped input that ends waits for background work before the session ends,
// and the result is drawn when it arrives.
func TestCLIEOFWaitsForBackground(t *testing.T) {
	m := &bgModelServer{childDelay: 600 * time.Millisecond}
	ws := bgWorkspace(t, m.start(t), "")
	cmd := mainHelper([]string{"-C", ws})
	cmd.Stdin = strings.NewReader("go\n")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if time.Since(start) < 600*time.Millisecond || !strings.Contains(out.String(), "background: child finished (completed") {
		t.Fatalf("the session ended before its background task:\n%s", out.String())
	}
}

// A line typed while a background task's ask waits, with no run live, is the
// answer to it when input is piped.
func TestCLIPipedIdleAskAnsweredByLine(t *testing.T) {
	m := &bgModelServer{childCommand: "touch made-by-child.txt"}
	ws := bgWorkspace(t, m.start(t), "")
	cmd := mainHelper([]string{"-C", ws})
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	_, _ = io.WriteString(in, "go\n")
	for deadline := time.Now().Add(20 * time.Second); !strings.Contains(out.String(), "answer 1-"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the child's ask never showed:\n%s", out.String())
		}
	}
	_, _ = io.WriteString(in, "1\n") // approvals are answered by number
	_ = in.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("never exited:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "made-by-child.txt")); err != nil {
		t.Fatalf("the answered ask did not run: %v\n%s", err, out.String())
	}
}

// At idle, only a line that is exactly a decision key answers a background
// task's waiting ask. Any other line goes to the model as a prompt, with a
// note that the approval still waits; it is never taken as the answer.
func TestCLIIdleLineIsAPromptUnlessADecision(t *testing.T) {
	m := &bgModelServer{childCommand: "touch made-by-child.txt"}
	ws := bgWorkspace(t, m.start(t), "")
	cmd := mainHelper([]string{"-C", ws})
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	waitOut := func(what string) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); !strings.Contains(out.String(), what); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("never saw %q:\n%s", what, out.String())
			}
		}
	}
	_, _ = io.WriteString(in, "go\n")
	waitOut("answer 1-")
	_, _ = io.WriteString(in, "yes please, and check the logs\n")
	waitOut("an approval is still waiting")
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, ok := m.prompted.Load("yes please, and check the logs"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the line never reached the model as a prompt:\n%s", out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "made-by-child.txt")); err == nil {
		t.Fatal("a line that is not a decision key answered the ask")
	}
	_, _ = io.WriteString(in, "1\n")
	_ = in.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("never exited:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "made-by-child.txt")); err != nil {
		t.Fatalf("the decision key did not answer the ask: %v\n%s", err, out.String())
	}
}

// During a run too, only a decision key answers the agent's waiting ask; any
// other piped line steers the run and says the approval still waits.
func TestCLIRunLineSteersUnlessADecision(t *testing.T) {
	m := &bgModelServer{parentCommand: "touch made-by-parent.txt"}
	ws := bgWorkspace(t, m.start(t), "")
	cmd := mainHelper([]string{"-C", ws})
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	waitOut := func(what string) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); !strings.Contains(out.String(), what); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("never saw %q:\n%s", what, out.String())
			}
		}
	}
	_, _ = io.WriteString(in, "run it\n")
	waitOut("answer 1-")
	_, _ = io.WriteString(in, "and keep it short\n")
	waitOut("it steers the run")
	if _, err := os.Stat(filepath.Join(ws, "made-by-parent.txt")); err == nil {
		t.Fatal("a line that is not a decision key answered the ask")
	}
	_, _ = io.WriteString(in, "1\n")
	_ = in.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("never exited:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "made-by-parent.txt")); err != nil {
		t.Fatalf("the decision key did not answer the ask: %v\n%s", err, out.String())
	}
}

// A wake the session started itself that finds its results already taken
// prints nothing; any other error, and an explicit wake's, still print.
func TestWakeWithNothingToDoIsSilent(t *testing.T) {
	if got := runErrorLine(agent.ErrNothingToWake, true); got != "" {
		t.Fatalf("a policy wake with nothing to do printed %q", got)
	}
	if got := runErrorLine(agent.ErrNothingToWake, false); got == "" {
		t.Fatal("an explicit wake with nothing to do said nothing")
	}
	if got := runErrorLine(errors.New("model down"), true); got != "model down" {
		t.Fatalf("a wake's real error: %q", got)
	}
	if runErrorLine(nil, false) != "" {
		t.Fatal("no error printed a line")
	}
}

// A turn that completed, or a wake stopped at its cap, runs on for a steer
// that arrived after its last look; an interrupted or failed one does not,
// nor one with nothing queued.
func TestTurnRunsOnForAQueuedSteer(t *testing.T) {
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, c := range []struct {
		name   string
		ctx    context.Context
		o      turnOutcome
		queued int
		want   bool
	}{
		{"completed", ctx, turnOutcome{reason: agent.TermCompleted}, 1, true},
		{"wake limit", ctx, turnOutcome{reason: agent.TermWakeLimit}, 1, true},
		{"nothing queued", ctx, turnOutcome{reason: agent.TermCompleted}, 0, false},
		{"interrupted", ctx, turnOutcome{reason: agent.TermUserInterrupt}, 1, false},
		{"max turns", ctx, turnOutcome{reason: agent.TermMaxTurns}, 1, false},
		{"error", ctx, turnOutcome{reason: agent.TermCompleted, err: errors.New("x")}, 1, false},
		{"cancelled", cancelled, turnOutcome{reason: agent.TermCompleted}, 1, false},
	} {
		if got := runsOnFor(c.ctx, c.o, c.queued); got != c.want {
			t.Errorf("%s: runs on %v, want %v", c.name, got, c.want)
		}
	}
}
