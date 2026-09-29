package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// bgModelServer is an OpenAI-compatible stub. A prompt starting "go" starts
// one background task ("child"); the child answers after childDelay, or runs
// childCommand first when one is set; a background result is answered
// "noted"; anything else "done".
type bgModelServer struct {
	childDelay   time.Duration
	childCommand string
	calls        atomic.Int64
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
	for deadline := time.Now().Add(20 * time.Second); !strings.Contains(out.String(), "[a]ccept"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the child's ask never showed:\n%s", out.String())
		}
	}
	_, _ = io.WriteString(in, "a\n")
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
