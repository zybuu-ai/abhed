package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// fakeLSP answers initialize and hover, and publishes a diagnostic on didOpen.
const fakeLSP = `import json, sys
def send(m):
    b = json.dumps(m).encode()
    sys.stdout.buffer.write(b"Content-Length: %d\r\n\r\n" % len(b) + b); sys.stdout.flush()
while True:
    n = 0
    while True:
        line = sys.stdin.buffer.readline()
        if not line: sys.exit(0)
        line = line.strip()
        if not line: break
        k, v = line.split(b":", 1)
        if k.lower() == b"content-length": n = int(v)
    m = json.loads(sys.stdin.buffer.read(n))
    if m.get("method") == "initialize": send({"jsonrpc":"2.0","id":m["id"],"result":{"capabilities":{"hoverProvider":True}}})
    elif m.get("method") == "textDocument/hover": send({"jsonrpc":"2.0","id":m["id"],"result":{"contents":{"kind":"markdown","value":"hover\nline"}}})
    elif m.get("method") == "textDocument/didOpen":
        send({"jsonrpc":"2.0","method":"textDocument/publishDiagnostics","params":{"uri":m["params"]["textDocument"]["uri"],"diagnostics":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"message":"bad","severity":1}]}})
`

func lspBench(t *testing.T, tier string, edit func(*config.Config)) (*workbench, string) {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	script := filepath.Join(t.TempDir(), "fake_lsp.py")
	if err := os.WriteFile(script, []byte(fakeLSP), 0o644); err != nil {
		t.Fatal(err)
	}
	wb := shellBenchOpts(t, func(c *config.Config) {
		c.Sandbox.LanguageServers = map[string][]string{"go": {py, script}}
		if edit != nil {
			edit(c)
		}
	}, func(o *Options) {
		// The host stands in for the sandbox; the label is what startLSP judges.
		sb := sandbox.NewNone(sandbox.DefaultPolicy(o.Workspace))
		bash := tools.Bash{Sandbox: sb.Command, Isolation: tools.Isolation{Tier: tier, Backend: "test"}}
		o.Registry = tools.NewRegistry(tools.Read{}, tools.Write{}, bash)
	})
	return wb, py
}

func lspSend(t *testing.T, srvURL, path string, msg any) {
	t.Helper()
	b, _ := json.Marshal(msg)
	req, _ := http.NewRequest("POST", srvURL+path, strings.NewReader(string(b)))
	req.Header.Set("X-Abhed-Tenant", "acme")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusNoContent {
		t.Fatalf("send %v: %v %v", msg, err, res)
	}
	_ = res.Body.Close()
}

// A configured language server runs in the session's sandbox as the
// person's recorded call, and its messages travel both ways.
func TestLanguageServerRunsAndAnswers(t *testing.T) {
	wb, py := lspBench(t, "process", nil)
	rec := wb.send("acme", "POST", "lsp", lspStartRequest{Language: "go"})
	var start lspStartResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &start) != nil || start.ID == "" || start.Root != wb.workspace {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	// The same language reuses it.
	var again lspStartResponse
	_ = json.Unmarshal(wb.send("acme", "POST", "lsp", lspStartRequest{Language: "go"}).Body.Bytes(), &again)
	if again.ID != start.ID {
		t.Fatalf("a second server was started: %s and %s", start.ID, again.ID)
	}

	srv := httptest.NewServer(wb.h)
	defer srv.Close()
	base := "/v1/sessions/" + wb.session + "/lsp/" + start.ID
	req, _ := http.NewRequest("GET", srv.URL+base, nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	res, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the deferred call below, once the stream is done
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	events := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				if raw, err := base64.StdEncoding.DecodeString(d); err == nil && len(raw) > 0 {
					events <- string(raw)
				}
			}
		}
		close(events)
	}()
	next := func() string {
		select {
		case e := <-events:
			return e
		case <-time.After(10 * time.Second):
			t.Fatal("no message from the language server")
		}
		return ""
	}
	time.Sleep(100 * time.Millisecond) // the stream is subscribed before the first message
	lspSend(t, srv.URL, base, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	if m := next(); !strings.Contains(m, `"hoverProvider": true`) {
		t.Fatalf("initialize: %s", m)
	}
	lspSend(t, srv.URL, base, map[string]any{"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": map[string]any{"textDocument": map[string]any{"uri": "file://" + wb.workspace + "/a.go", "text": "x"}}})
	if m := next(); !strings.Contains(m, "publishDiagnostics") || !strings.Contains(m, `"bad"`) {
		t.Fatalf("diagnostics: %s", m)
	}
	lspSend(t, srv.URL, base, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "textDocument/hover", "params": map[string]any{}})
	if m := next(); !strings.Contains(m, `hover\nline`) {
		t.Fatalf("hover: %s", m)
	}
	// Starting it was the person's call, recorded with its command line.
	asked := false
	for _, e := range wb.events() {
		asked = asked || e.Type == agent.EvActionRequested && e.Actor == agent.ActorUser && strings.Contains(string(e.Payload), "language server for go") && strings.Contains(string(e.Payload), py)
	}
	if !asked {
		t.Fatal("the language server's start is not in the record")
	}
	if rec := wb.send("acme", "DELETE", "lsp/"+start.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("stop: %d", rec.Code)
	}
}

// No sandbox, no configuration, or a deny rule: no language server.
func TestLanguageServerRefusals(t *testing.T) {
	wb, _ := lspBench(t, "none", nil)
	if rec := wb.send("acme", "POST", "lsp", lspStartRequest{Language: "go"}); rec.Code != http.StatusConflict {
		t.Errorf("on the none tier: %d %s", rec.Code, rec.Body)
	}
	for _, tier := range []string{"", "host", "None"} {
		wb, _ = lspBench(t, tier, nil)
		if rec := wb.send("acme", "POST", "lsp", lspStartRequest{Language: "go"}); rec.Code != http.StatusConflict {
			t.Errorf("on tier %q: %d %s", tier, rec.Code, rec.Body)
		}
	}
	wb, _ = lspBench(t, "process", nil)
	if rec := wb.send("acme", "POST", "lsp", lspStartRequest{Language: "python"}); rec.Code != http.StatusNotFound {
		t.Errorf("an unconfigured language: %d %s", rec.Code, rec.Body)
	}
	wb, _ = lspBench(t, "process", func(c *config.Config) { c.Permissions.Mode = "plan" })
	if rec := wb.send("acme", "POST", "lsp", lspStartRequest{Language: "go"}); rec.Code != http.StatusForbidden {
		t.Errorf("in plan mode: %d %s", rec.Code, rec.Body)
	}
	wb, _ = lspBench(t, "process", func(c *config.Config) { c.Permissions.Deny = append(c.Permissions.Deny, "bash(*fake_lsp*)") })
	if rec := wb.send("acme", "POST", "lsp", lspStartRequest{Language: "go"}); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Denied") {
		t.Errorf("a denied command: %d %s", rec.Code, rec.Body)
	}
}

func TestLSPFraming(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("Content-Type: x\r\nContent-Length: 2\r\n\r\n{}Content-Length: 99999999999\r\n\r\n"))
	if m, err := readLSPMessage(br); err != nil || string(m) != "{}" {
		t.Fatalf("%q %v", m, err)
	}
	if _, err := readLSPMessage(br); err == nil {
		t.Fatal("an oversized message was read")
	}
}

// A language server's header line is bounded: one with no end, or one too
// long, is an error rather than memory the host keeps growing.
func TestLSPHeaderBounded(t *testing.T) {
	for name, in := range map[string]string{
		"no newline":   strings.Repeat("x", 200<<10),
		"long line":    "X-Pad: " + strings.Repeat("x", 4<<10) + "\r\nContent-Length: 2\r\n\r\n{}",
		"many headers": strings.Repeat("X-Pad: 1\r\n", 100) + "Content-Length: 2\r\n\r\n{}",
	} {
		br := bufio.NewReaderSize(strings.NewReader(in), 64<<10)
		if _, err := readLSPMessage(br); err == nil || !strings.Contains(err.Error(), "header too long") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A page that does not read is ended once its queue passes lspQueue; the
// server's output is not held for it without bound.
func TestLSPSlowReaderEnded(t *testing.T) {
	body := `{"x":"` + strings.Repeat("a", 1<<20) + `"}`
	frame := "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	run := &lspRun{cancel: func() {}, subs: map[*lspSub]struct{}{}}
	sub := &lspSub{ch: make(chan []byte, 256), behind: make(chan struct{})}
	run.subs[sub] = struct{}{}
	run.read(strings.NewReader(strings.Repeat(frame, 40)))
	select {
	case <-sub.behind:
	default:
		t.Fatal("a reader 40 MiB behind was kept")
	}
	if len(sub.ch)*len(body) > lspQueue {
		t.Fatalf("%d messages queued", len(sub.ch))
	}
}

// Only what the editor sends reaches a language server: executeCommand and
// the like would run programs the policy never judged.
func TestLSPMethodAllowlist(t *testing.T) {
	for _, m := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"textDocument/hover","params":{}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{}}`,
		`{"jsonrpc":"2.0","id":7,"result":null}`,
		`{"jsonrpc":"2.0","id":8,"result":[null,null]}`,
	} {
		if _, ok := lspAllowed([]byte(m), "/w"); !ok {
			t.Errorf("refused %s", m)
		}
	}
	for _, m := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"workspace/executeCommand","params":{"command":"gopls.generate"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"workspace/executeCommand","METHOD":"textDocument/hover"}`,
		`{"jsonrpc":"2.0","id":1,"method":"textDocument/hover","method":"workspace/executeCommand"}`,
		`[{"jsonrpc":"2.0","id":1,"method":"workspace/executeCommand"}]`,
		`{"jsonrpc":"2.0","id":1,"method":"textDocument/codeAction"}`,
		`{"jsonrpc":"2.0","id":null,"result":null}`,
		`{"jsonrpc":"2.0","result":null}`,
		`{"jsonrpc":"2.0","id":"a","error":{"code":1,"message":"x"}}`,
		`{"jsonrpc":"2.0","id":7,"result":[{"env":{"GOPACKAGESDRIVER":"/tmp/x"}}]}`,
		`{"jsonrpc":"2.0","id":7,"result":[null,{"buildFlags":["-toolexec=/tmp/x"]}]}`,
		`{"jsonrpc":"2.0","id":7,"result":{}}`,
		`{"jsonrpc":"2.0","id":7}`,
	} {
		if out, ok := lspAllowed([]byte(m), "/w"); ok {
			t.Errorf("allowed %s as %s", m, out)
		}
	}
	// A duplicate key reaches the server once, as checked.
	if out, ok := lspAllowed([]byte(`{"method":"workspace/executeCommand","method":"textDocument/hover","id":1}`), "/w"); ok && strings.Contains(string(out), "executeCommand") {
		t.Errorf("sent %s", out)
	}

	wb, _ := lspBench(t, "process", nil)
	var start lspStartResponse
	_ = json.Unmarshal(wb.send("acme", "POST", "lsp", lspStartRequest{Language: "go"}).Body.Bytes(), &start)
	srv := httptest.NewServer(wb.h)
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/sessions/"+wb.session+"/lsp/"+start.ID,
		strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"workspace/executeCommand","params":{"command":"x"}}`))
	req.Header.Set("X-Abhed-Tenant", "acme")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("executeCommand: %d", res.StatusCode)
	}
	wb.send("acme", "DELETE", "lsp/"+start.ID, nil)
}

// initialize reaches the server as the editor's fixed shape: options that
// set its environment or tools, and a root elsewhere, are not passed on.
func TestLSPInitializeRebuilt(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"processId":1,"rootUri":"file:///etc","initializationOptions":{"env":{"GOPACKAGESDRIVER":"/tmp/x"}},"capabilities":{"experimental":{"x":1}},"trace":"verbose"}}`
	out, ok := lspAllowed([]byte(in), "/home/a b/w")
	if !ok {
		t.Fatal("initialize refused")
	}
	var m struct {
		ID     int                        `json:"id"`
		Method string                     `json:"method"`
		Params map[string]json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(out, &m); err != nil || m.ID != 1 || m.Method != "initialize" {
		t.Fatalf("%s %v", out, err)
	}
	want := map[string]string{
		"processId":        `null`,
		"rootUri":          `"file:///home/a%20b/w"`,
		"workspaceFolders": `[{"name":"w","uri":"file:///home/a%20b/w"}]`,
		"capabilities":     lspCapabilities,
	}
	if len(m.Params) != len(want) {
		t.Fatalf("params %s", out)
	}
	for k, v := range want {
		if string(m.Params[k]) != v {
			t.Errorf("%s = %s, want %s", k, m.Params[k], v)
		}
	}
	if out, _ := lspAllowed([]byte(`{"jsonrpc":"2.0","method":"initialized","params":{"settings":{"env":{}}}}`), "/w"); !strings.Contains(string(out), `"params":{}`) {
		t.Errorf("initialized: %s", out)
	}
}

// With a real gopls, an environment slipped into initialize or into an answer
// to its configuration request does not run a program.
func TestLSPGoplsRunsNothingThePageNames(t *testing.T) {
	gopls, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls not installed")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	marker := filepath.Join(t.TempDir(), "ran")
	driver := filepath.Join(t.TempDir(), "driver.sh")
	if err := os.WriteFile(driver, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	wb, _ := lspBench(t, "process", func(c *config.Config) {
		c.Sandbox.LanguageServers = map[string][]string{"go": {gopls}}
	})
	for name, body := range map[string]string{"go.mod": "module m\n\ngo 1.21\n", "a.go": "package m\n\nfunc A() {}\n"} {
		if err := os.WriteFile(filepath.Join(wb.workspace, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var start lspStartResponse
	if rec := wb.send("acme", "POST", "lsp", lspStartRequest{Language: "go"}); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &start) != nil {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	defer wb.send("acme", "DELETE", "lsp/"+start.ID, nil)
	srv := httptest.NewServer(wb.h)
	defer srv.Close()
	base := "/v1/sessions/" + wb.session + "/lsp/" + start.ID
	req, _ := http.NewRequest("GET", srv.URL+base, nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	res, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the deferred call below
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	events := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				if raw, err := base64.StdEncoding.DecodeString(d); err == nil && len(raw) > 0 {
					events <- string(raw)
				}
			}
		}
		close(events)
	}()
	time.Sleep(100 * time.Millisecond)
	post := func(msg string) int {
		req, _ := http.NewRequest("POST", srv.URL+base, strings.NewReader(msg))
		req.Header.Set("X-Abhed-Tenant", "acme")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	env, _ := json.Marshal(map[string]any{"env": map[string]string{"GOPACKAGESDRIVER": driver, "GOFLAGS": "-toolexec=" + driver}})
	uri := lspURI(wb.workspace)
	post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"processId":null,"rootUri":"` + uri + `","initializationOptions":` + string(env) + `,"capabilities":{"workspace":{"configuration":true}}}}`)
	post(`{"jsonrpc":"2.0","method":"initialized","params":{}}`)
	post(`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"` + uri + `/a.go","languageId":"go","version":1,"text":"package m\n\nfunc A() {}\n"}}}`)
	deadline := time.After(60 * time.Second)
	for loaded := false; !loaded; {
		select {
		case m, open := <-events:
			if !open {
				t.Fatal("the stream ended")
			}
			var in struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			_ = json.Unmarshal([]byte(m), &in)
			if in.Method == "workspace/configuration" {
				if code := post(`{"jsonrpc":"2.0","id":` + string(in.ID) + `,"result":[` + string(env) + `]}`); code != http.StatusForbidden {
					t.Errorf("a configuration answer with env: %d", code)
				}
				post(`{"jsonrpc":"2.0","id":` + string(in.ID) + `,"result":[null]}`)
			} else if in.ID != nil && in.Method != "" {
				post(`{"jsonrpc":"2.0","id":` + string(in.ID) + `,"result":null}`)
			}
			loaded = in.Method == "textDocument/publishDiagnostics" && strings.Contains(m, "a.go")
		case <-deadline:
			t.Fatal("gopls published no diagnostics for a.go")
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("gopls ran a program the page named")
	}
}

// Closing the session records each language server's end before the
// session's own: the start was authorized and recorded, so its result is too.
func TestLanguageServerEndRecordedBeforeSessionEnd(t *testing.T) {
	wb, _ := lspBench(t, "process", nil)
	var start lspStartResponse
	if rec := wb.send("acme", "POST", "lsp", lspStartRequest{Language: "go"}); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &start) != nil {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	wb.s.closeIdle(time.Now().Add(10 * time.Second))
	observed, ended := -1, -1
	for i, e := range wb.events() {
		switch {
		case e.Type == agent.EvObservation && strings.Contains(string(e.Payload), start.ID):
			observed = i
		case e.Type == agent.EvSessionEnded && ended < 0:
			ended = i
		}
	}
	if observed < 0 || ended < 0 || observed > ended {
		t.Fatalf("language server's end at %d, session's end at %d", observed, ended)
	}
}

// A start whose pipes cannot be made is still answered on the record.
func TestLanguageServerPipeFailureRecorded(t *testing.T) {
	wb, _ := lspBench(t, "process", nil)
	wb.s.mu.Lock()
	live := wb.s.running[wb.session]
	wb.s.mu.Unlock()
	bash, _ := live.Loop.Tools.Get("bash")
	b := bash.(tools.Bash)
	b.Sandbox = func(ctx context.Context, cwd, command string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "true")
		cmd.Stdout = io.Discard // StdoutPipe refuses a command with Stdout set
		return cmd
	}
	live.Loop.Tools = tools.NewRegistry(tools.Read{}, tools.Write{}, b)
	if rec := wb.send("acme", "POST", "lsp", lspStartRequest{Language: "go"}); rec.Code != http.StatusInternalServerError {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	asked, answered := "", false
	for _, e := range wb.events() {
		var p struct {
			CallID  string `json:"call_id"`
			IsError bool   `json:"is_error"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if e.Type == agent.EvActionRequested && strings.Contains(string(e.Payload), "language server for go") {
			asked = p.CallID
		}
		answered = answered || e.Type == agent.EvObservation && asked != "" && p.CallID == asked && p.IsError
	}
	if asked == "" || !answered {
		t.Fatalf("asked %q, answered %v", asked, answered)
	}
}

// A definition's URI with '..' in it lands where it points, and one that
// climbs out of the workspace opens nothing.
func TestIDELanguageServerPaths(t *testing.T) {
	if out, err := runConsoleCases(t, "ide-lsp", "", "ide_lsp_cases.mjs"); err != nil {
		t.Fatalf("the workbench mapped a language server's path wrongly:\n%s", out)
	}
}

// A language server runs with GOTOOLCHAIN=local, so a go.mod toolchain line
// cannot have the go command fetch and run another Go; the record says so.
func TestLanguageServerKeepsTheLocalGo(t *testing.T) {
	wb, _ := lspBench(t, "process", func(c *config.Config) {
		c.Sandbox.LanguageServers = map[string][]string{"go": {"/bin/sh", "-c", `printf %s "$GOTOOLCHAIN" > tc.txt; sleep 30`}}
	})
	var start lspStartResponse
	if rec := wb.send("acme", "POST", "lsp", lspStartRequest{Language: "go"}); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &start) != nil {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	defer wb.send("acme", "DELETE", "lsp/"+start.ID, nil)
	var got []byte
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if got, _ = os.ReadFile(filepath.Join(wb.workspace, "tc.txt")); len(got) > 0 {
			break
		}
	}
	if string(got) != "local" {
		t.Fatalf("GOTOOLCHAIN in the language server: %q", got)
	}
	said := false
	for _, e := range wb.events() {
		said = said || e.Type == agent.EvActionRequested && strings.Contains(string(e.Payload), "run with GOTOOLCHAIN=local")
	}
	if !said {
		t.Fatal("the record does not say the language server runs with GOTOOLCHAIN=local")
	}
}
