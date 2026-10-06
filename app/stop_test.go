//go:build unix

package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
)

// TestStopHelper is the command a test below starts and signals: rpc or eval,
// in the workspace it names, exiting with the command's own status.
func TestStopHelper(t *testing.T) {
	ws := os.Getenv("ABHED_STOP_WS")
	switch os.Getenv("ABHED_STOP_HELPER") {
	case "rpc":
		os.Exit(rpcCmd(ws, ""))
	case "eval":
		os.Exit(evalCmd(ws, filepath.Join(ws, "corpus"), filepath.Join(ws, "report.json"), ""))
	case "acp":
		os.Exit(acpCmd(ws, acpBuild{Version: "test", Edition: "ce"}, ""))
	case "p":
		os.Exit(Main([]string{"-C", ws, "-p", "go"}))
	}
	t.Skip("run by the stop tests")
}

// stubModel answers the first request with the call and every later one by
// waiting until the client goes away; entered closes when a request arrives.
func stubModel(t *testing.T, call string) (url string, entered <-chan struct{}) {
	t.Helper()
	in := make(chan struct{})
	var once, first sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the body first: only then does the server notice the client leave.
		_, _ = io.Copy(io.Discard, r.Body)
		once.Do(func() { close(in) })
		sent := false
		first.Do(func() {
			if call == "" {
				return
			}
			sent = true
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"bash","arguments":`+strconv.Quote(call)+`}}]}}]}`)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		})
		if !sent {
			<-r.Context().Done()
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, in
}

// stopWorkspace writes a workspace whose model is url, and starts the helper
// in it as mode; the helper is this test's own child, signalled only by pid.
func stopWorkspace(t *testing.T, url, mode string) (string, *exec.Cmd, io.WriteCloser, *strings.Builder) {
	t.Helper()
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	cfg := `{"permissions":{"allow":["bash(*)"]},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + url + `","model":"m","context_window":8192}}}}`
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.TrustEnv, "1") // the test wrote this configuration
	helper := exec.Command(os.Args[0], "-test.run=^TestStopHelper$")
	helper.Env = append(os.Environ(), "ABHED_STOP_HELPER="+mode, "ABHED_STOP_WS="+ws, "HOME="+t.TempDir())
	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out := &strings.Builder{}
	helper.Stdout = out
	helper.Stderr = out
	return ws, helper, stdin, out
}

func exitOf(t *testing.T, helper *exec.Cmd) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- helper.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	case <-time.After(15 * time.Second):
		_ = helper.Process.Kill()
		<-done
		t.Fatal("the helper did not exit after the signal")
		return 0
	}
}

// rpc stopped by SIGTERM ends the command it was running and exits as the
// signal would have ended it.
func TestRPCExitsOnSIGTERMAndEndsItsCommand(t *testing.T) {
	url, _ := stubModel(t, beatingCommand)
	ws, helper, stdin, out := stopWorkspace(t, url, "rpc")
	beat := filepath.Join(ws, "beat")
	requireHostTier(t, ws)
	stderr := &strings.Builder{}
	helper.Stderr = stderr // stdout alone is the protocol
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(stdin, `{"method":"start","workspace":%q,"allow":["bash(*)"]}`+"\n", ws)
	fmt.Fprint(stdin, `{"id":"1","method":"prompt","prompt":"go"}`+"\n")

	if !waitBeating(beat) {
		_ = helper.Process.Kill()
		_ = helper.Wait()
		t.Fatalf("the command never started:\n%s", out)
	}
	_ = helper.Process.Signal(syscall.SIGTERM)
	if code := exitOf(t, helper); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("rpc exited %d, want %d:\n%s", code, 128+int(syscall.SIGTERM), out)
	}
	// stdout is the caller's only record: the run's end, then the prompt's reply, last.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	ended, reply := -1, -1
	for i, l := range lines {
		if strings.Contains(l, `"type":"session.ended"`) {
			ended = i
		}
		if strings.Contains(l, `"id":"1"`) {
			reply = i
		}
	}
	if ended < 0 || reply != len(lines)-1 || ended > reply {
		t.Fatalf("session.ended at %d and the reply at %d of %d lines:\n%s\nstderr:\n%s", ended, reply, len(lines), out, stderr)
	}
	if !stopsBeating(beat) {
		t.Fatal("the command outlived rpc")
	}
}

// eval stopped part way prints no summary, writes no report, and exits as
// the signal would have ended it.
func TestEvalStoppedWritesNoReport(t *testing.T) {
	url, entered := stubModel(t, "")
	ws, helper, _, out := stopWorkspace(t, url, "eval")
	var tasks []string
	for i := 1; i <= 3; i++ {
		tasks = append(tasks, fmt.Sprintf(`{"id":"t%d","prompt":"go","files":{"a.txt":"x"},"assertions":[{"type":"file_exists","path":"a.txt"}]}`, i))
	}
	if err := os.MkdirAll(filepath.Join(ws, "corpus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "corpus", "tasks.json"), []byte("["+strings.Join(tasks, ",")+"]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		_ = helper.Process.Kill()
		_ = helper.Wait()
		t.Fatalf("eval never called the model:\n%s", out)
	}
	_ = helper.Process.Signal(os.Interrupt)
	if code := exitOf(t, helper); code != 128+int(syscall.SIGINT) {
		t.Fatalf("eval exited %d, want %d:\n%s", code, 128+int(syscall.SIGINT), out)
	}
	if _, err := os.Stat(filepath.Join(ws, "report.json")); !os.IsNotExist(err) {
		t.Fatal("a stopped eval wrote its report")
	}
	if s := out.String(); strings.Contains(s, "PASS") || strings.Contains(s, "success") || !strings.Contains(s, "no summary or report written") {
		t.Fatalf("a stopped eval reported results:\n%s", s)
	}
}

// acp stopped by SIGTERM sends every update of the stopped prompt, then its
// stopReason last, and exits as the signal would have ended it.
func TestACPExitsOnSIGTERMAfterTheStopReason(t *testing.T) {
	url, _ := stubModel(t, beatingCommand)
	ws, helper, stdin, _ := stopWorkspace(t, url, "acp")
	beat := filepath.Join(ws, "beat")
	requireHostTier(t, ws)
	helper.Stdout = nil
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	send := func(id int, method string, params any) {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		_, _ = stdin.Write(append(raw, '\n'))
	}
	next := func(match func(string) bool) string {
		deadline := time.After(20 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatal("acp ended early")
				}
				if match(l) {
					return l
				}
			case <-deadline:
				_ = helper.Process.Kill()
				t.Fatal("no answer from acp")
			}
		}
	}
	send(1, "initialize", map[string]any{"protocolVersion": 1})
	next(func(l string) bool { return strings.Contains(l, `"id":1`) })
	send(2, "session/new", map[string]any{"cwd": ws, "mcpServers": []any{}})
	var created struct {
		Result struct {
			SessionID string `json:"sessionId"`
		} `json:"result"`
	}
	_ = json.Unmarshal([]byte(next(func(l string) bool { return strings.Contains(l, `"id":2`) })), &created)
	send(3, "session/prompt", map[string]any{"sessionId": created.Result.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "go"}}})

	if !waitBeating(beat) {
		_ = helper.Process.Kill()
		t.Fatal("the command never started")
	}
	_ = helper.Process.Signal(syscall.SIGTERM)
	var rest []string
	for l := range lines {
		rest = append(rest, l)
	}
	if code := exitOf(t, helper); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("acp exited %d, want %d", code, 128+int(syscall.SIGTERM))
	}
	if len(rest) == 0 || !strings.Contains(rest[len(rest)-1], `"stopReason":"cancelled"`) {
		t.Fatalf("the stopReason is not the last message:\n%s", strings.Join(rest, "\n"))
	}
	if !stopsBeating(beat) {
		t.Fatal("the command outlived acp")
	}
}

// beatingCommand is the model's bash call: a command that writes a rising
// count to beat in the workspace every 0.1 s, for 60 s at most. Whether it
// still runs is read from the file, which every tier shows: a pid it read
// under bwrap was its namespace's, not the host's.
const beatingCommand = `{"command":"for i in $(seq 1 600); do echo $i > beat; sleep 0.1; done; true","description":"long"}`

// waitBeating waits for the command to start writing beat.
func waitBeating(beat string) bool {
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(beat); err == nil && len(b) > 0 {
			return true
		}
	}
	return false
}

// stopsBeating reports whether beat stops changing within a few seconds.
func stopsBeating(beat string) bool {
	last, _ := os.ReadFile(beat)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		time.Sleep(time.Second)
		now, _ := os.ReadFile(beat)
		if string(now) == string(last) {
			return true
		}
		last = now
	}
	return false
}

// requireHostTier skips where rpc and acp would run the command somewhere
// stopping it works otherwise: the container or vm tier, or a sandbox this machine
// cannot start.
func requireHostTier(t *testing.T, ws string) {
	t.Helper()
	sb, err := sandboxconfig.Build(config.Config{}, ws)
	if err != nil {
		t.Skipf("no sandbox here: %v", err)
	}
	if sb.Tier() == sandbox.TierContainer || sb.Tier() == sandbox.TierVM {
		t.Skipf("the %s tier gives the command a pid namespace of its own", sb.Tier())
	}
	if out, err := sb.Command(t.Context(), ws, "true").CombinedOutput(); err != nil {
		t.Skipf("the %s tier cannot run a command here: %v: %s", sb.Tier(), err, out)
	}
}

// A stop signal while -p is starting up, held in an MCP connect, ends it
// with 128 plus the signal's number, as a shell reports it, not by the
// signal's default action.
func TestPromptStoppedDuringStartupExitsBySignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			url, _ := stubModel(t, "")
			entered := make(chan struct{})
			var once sync.Once
			mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				once.Do(func() { close(entered) })
				time.Sleep(time.Second)
				http.Error(w, "not now", http.StatusServiceUnavailable)
			}))
			t.Cleanup(mcpSrv.Close)
			ws, helper, _, out := stopWorkspace(t, url, "p")
			cfg := `{"mcp":{"servers":[{"name":"slow","url":"` + mcpSrv.URL + `","enabled":true}]},` +
				`"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + url + `","model":"m","context_window":8192}}}}`
			if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := helper.Start(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(20 * time.Second):
				_ = helper.Process.Kill()
				_ = helper.Wait()
				t.Fatalf("start-up never reached the MCP server:\n%s", out)
			}
			_ = helper.Process.Signal(sig)
			if code := exitOf(t, helper); code != 128+int(sig) {
				t.Fatalf("exited %d, want %d:\n%s", code, 128+int(sig), out)
			}
		})
	}
}

// TestStreamStopHelper runs -p reading stream-json from stdin, for the tests below.
func TestStreamStopHelper(t *testing.T) {
	ws := os.Getenv("ABHED_STOP_WS")
	if os.Getenv("ABHED_STOP_HELPER") != "pstream" {
		t.Skip("run by the stop tests")
	}
	os.Exit(Main([]string{"-C", ws, "-p", "-input-format", "stream-json", "-output-format", "stream-json"}))
}

// textModel answers every request with reply; entered closes at the first.
func textModel(t *testing.T, reply string) (string, <-chan struct{}) {
	t.Helper()
	in := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		once.Do(func() { close(in) })
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", strconv.Quote(reply))
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL, in
}

// lockedBuilder is a strings.Builder safe to read while the helper writes it.
type lockedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuilder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func startStream(t *testing.T, url string) (*exec.Cmd, io.WriteCloser, *lockedBuilder) {
	t.Helper()
	_, helper, stdin, _ := stopWorkspace(t, url, "pstream")
	helper.Args = []string{os.Args[0], "-test.run=^TestStreamStopHelper$"}
	out := &lockedBuilder{}
	helper.Stdout, helper.Stderr = out, out
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, `{"type":"user","message":{"content":"first"}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	return helper, stdin, out
}

func resultOf(t *testing.T, out string) resultLine {
	t.Helper()
	var res resultLine
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, `{"type":"result"`) {
			if err := json.Unmarshal([]byte(line), &res); err != nil {
				t.Fatal(err)
			}
		}
	}
	if res.Type != "result" {
		t.Fatalf("no result line:\n%s", out)
	}
	return res
}

// A stream-json -p run whose stdin is still open ends on SIGTERM, mid-turn
// or waiting for the next message, with 143 and a result line.
func TestStreamInputStopsOnSignalWithStdinOpen(t *testing.T) {
	t.Run("mid-turn", func(t *testing.T) {
		url, entered := stubModel(t, "")
		helper, stdin, out := startStream(t, url)
		defer stdin.Close()
		select {
		case <-entered:
		case <-time.After(20 * time.Second):
			_ = helper.Process.Kill()
			_ = helper.Wait()
			t.Fatalf("the turn never reached the model:\n%s", out)
		}
		_ = helper.Process.Signal(syscall.SIGTERM)
		if code := exitOf(t, helper); code != 143 {
			t.Fatalf("exited %d:\n%s", code, out)
		}
		if res := resultOf(t, out.String()); res.ExitCode != 143 {
			t.Fatalf("result %+v", res)
		}
	})
	t.Run("between turns", func(t *testing.T) {
		url, _ := textModel(t, "first-answer")
		helper, stdin, out := startStream(t, url)
		defer stdin.Close()
		deadline := time.Now().Add(20 * time.Second)
		for !strings.Contains(out.String(), "first-answer") {
			if time.Now().After(deadline) {
				_ = helper.Process.Kill()
				_ = helper.Wait()
				t.Fatalf("no answer:\n%s", out)
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = helper.Process.Signal(syscall.SIGTERM)
		if code := exitOf(t, helper); code != 143 {
			t.Fatalf("exited %d:\n%s", code, out)
		}
		resultOf(t, out.String())
	})
	t.Run("after a failure", func(t *testing.T) {
		helper, stdin, out := startStream(t, "http://127.0.0.1:9/v1")
		defer stdin.Close()
		// Stops reading after the failed turn, without waiting for stdin to end.
		if code := exitOf(t, helper); code == 0 {
			t.Fatalf("exited 0:\n%s", out)
		}
		if res := resultOf(t, out.String()); !res.IsError {
			t.Fatalf("result %+v", res)
		}
	})
}

// A -p run stopped mid-turn exits with 128 plus the signal and says so in its result.
func TestPromptStoppedMidTurnExitsBySignal(t *testing.T) {
	url, entered := stubModel(t, "")
	_, helper, stdin, out := stopWorkspace(t, url, "p")
	_ = stdin.Close()
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		_ = helper.Process.Kill()
		_ = helper.Wait()
		t.Fatalf("the turn never reached the model:\n%s", out)
	}
	_ = helper.Process.Signal(syscall.SIGINT)
	if code := exitOf(t, helper); code != 130 {
		t.Fatalf("exited %d, want 130:\n%s", code, out)
	}
}
