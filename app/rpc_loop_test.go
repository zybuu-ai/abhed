package app

import (
	"bufio"
	"context"
	"encoding/json"
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
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// globModel answers every request with one glob call, after hold returns, and
// keeps each request body.
type globModel struct {
	mu     sync.Mutex
	bodies []string
	hold   func(n int)
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

// rpcSession runs abhed rpc on pipes the test writes to and reads from.
type rpcSession struct {
	in    *os.File
	lines chan string
	done  chan struct{}
}

func startRPC(t *testing.T, ws string) *rpcSession {
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
	s := &rpcSession{in: inW, lines: make(chan string, 4096), done: make(chan struct{})}
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

func (s *rpcSession) send(line string) { fmt.Fprintln(s.in, line) }

// waitFor reads output until a line holds want, returning everything read.
func (s *rpcSession) waitFor(t *testing.T, want string) string {
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
	steered := make(chan struct{})
	m := &globModel{}
	// The first turn waits for the steer to be acknowledged.
	m.hold = func(n int) {
		if n == 1 {
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
