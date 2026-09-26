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
		os.Exit(rpcCmd(ws))
	case "eval":
		os.Exit(evalCmd(ws, filepath.Join(ws, "corpus"), filepath.Join(ws, "report.json")))
	case "acp":
		os.Exit(acpCmd(ws, "test"))
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
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	url, _ := stubModel(t, `{"command":"sh -c 'echo $$ > `+pidFile+`; exec sleep 60'; true","description":"long"}`)
	ws, helper, stdin, out := stopWorkspace(t, url, "rpc")
	requireHostTier(t, ws)
	stderr := &strings.Builder{}
	helper.Stderr = stderr // stdout alone is the protocol
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(stdin, `{"method":"start","workspace":%q,"allow":["bash(*)"]}`+"\n", ws)
	fmt.Fprint(stdin, `{"id":"1","method":"prompt","prompt":"go"}`+"\n")

	var pid int
	for deadline := time.Now().Add(20 * time.Second); pid == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = helper.Process.Kill()
			_ = helper.Wait()
			t.Fatalf("the command never started:\n%s", out)
		}
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	t.Cleanup(func() {
		if b, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output(); err == nil && strings.Contains(string(b), "sleep 60") {
			_ = syscall.Kill(pid, syscall.SIGKILL) // only the sleep this test's helper started
		}
	})
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
	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the command %d outlived rpc", pid)
		}
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
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	url, _ := stubModel(t, `{"command":"sh -c 'echo $$ > `+pidFile+`; exec sleep 60'; true","description":"long"}`)
	ws, helper, stdin, _ := stopWorkspace(t, url, "acp")
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

	var pid int
	for deadline := time.Now().Add(20 * time.Second); pid == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = helper.Process.Kill()
			t.Fatal("the command never started")
		}
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	t.Cleanup(func() {
		if b, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output(); err == nil && strings.Contains(string(b), "sleep 60") {
			_ = syscall.Kill(pid, syscall.SIGKILL) // only the sleep this test's helper started
		}
	})
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
	if syscall.Kill(pid, 0) == nil {
		time.Sleep(2 * time.Second)
		if syscall.Kill(pid, 0) == nil {
			t.Fatalf("the command %d outlived acp", pid)
		}
	}
}

// requireHostTier skips where rpc and acp would run the command somewhere its
// pid is not the host's: a container, or a sandbox this machine cannot start.
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
