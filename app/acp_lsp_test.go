package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/lsprelay"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/monitor"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// TestLSPFakeServer is a language server the LSP tests run in place of
// gopls: it answers initialize and hover, and once initialized asks to open
// an external page and reports diagnostics inside and outside the workspace.
// With ABHED_FAKE_LSP_CRASH naming a file not yet there, it makes the file
// and dies after initializing, once.
func TestLSPFakeServer(t *testing.T) {
	if os.Getenv("ABHED_FAKE_LSP") == "" {
		t.Skip("run by the LSP tests")
	}
	br := bufio.NewReader(os.Stdin)
	send := func(v map[string]any) {
		v["jsonrpc"] = "2.0"
		b, _ := json.Marshal(v)
		_ = lsprelay.Write(os.Stdout, b)
	}
	root := os.Getenv("ABHED_FAKE_LSP_ROOT")
	for {
		raw, err := lsprelay.Read(br)
		if err != nil {
			os.Exit(0)
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		method, _ := m["method"].(string)
		switch {
		case method == "initialize":
			params, _ := m["params"].(map[string]any)
			opts, hasOpts := params["initializationOptions"]
			send(map[string]any{"id": m["id"], "result": map[string]any{"capabilities": map[string]any{"hoverProvider": true, "fakeOptions": hasOpts, "initOptions": opts}}})
		case method == "initialized":
			if f := os.Getenv("ABHED_FAKE_LSP_CRASH"); f != "" {
				if _, err := os.Stat(f); err != nil {
					_ = os.WriteFile(f, nil, 0o600)
					os.Exit(2)
				}
			}
			if os.Getenv("ABHED_FAKE_LSP_CONFIG") != "" {
				send(map[string]any{"id": "c1", "method": "workspace/configuration", "params": map[string]any{"items": []any{map[string]any{"section": "rust-analyzer"}}}})
			}
			send(map[string]any{"id": "s1", "method": "window/showDocument", "params": map[string]any{"uri": "https://evil.example", "external": true}})
			send(map[string]any{"method": "textDocument/publishDiagnostics", "params": map[string]any{"uri": "file:///etc/passwd", "diagnostics": []any{}}})
			send(map[string]any{"method": "textDocument/publishDiagnostics", "params": map[string]any{"uri": "file://" + root + "/a.go", "diagnostics": []any{}}})
		case m["id"] == "c1" && method == "":
			b, _ := json.Marshal(m["result"])
			send(map[string]any{"method": "window/logMessage", "params": map[string]any{"type": 3, "message": "configuration answered " + string(b)}})
		case m["id"] == "s1" && method == "":
			send(map[string]any{"method": "window/logMessage", "params": map[string]any{"type": 3, "message": fmt.Sprintf("showDocument answered %v", m["result"])}})
		case method == "textDocument/didChange":
			params, _ := m["params"].(map[string]any)
			doc, _ := params["textDocument"].(map[string]any)
			send(map[string]any{"method": "window/logMessage", "params": map[string]any{"type": 4, "message": fmt.Sprintf("version %v", doc["version"])}})
		case method == "textDocument/hover":
			send(map[string]any{"id": m["id"], "result": map[string]any{"contents": "fake hover"}})
		case method == "workspace/executeCommand":
			send(map[string]any{"method": "window/logMessage", "params": map[string]any{"type": 1, "message": "executeCommand reached the server"}})
			send(map[string]any{"id": m["id"], "result": nil})
		case method == "shutdown":
			send(map[string]any{"id": m["id"], "result": nil})
		case method == "exit":
			os.Exit(0)
		}
	}
}

func fakeLSP(t *testing.T, root string, env ...string) *[]string {
	t.Helper()
	var asked []string
	oldCmd, oldTier, oldBackoff := lspCommand, lspTier, lspBackoff
	lspTier = func(tools.Bash) string { return "process" }
	lspBackoff = func(int) time.Duration { return 10 * time.Millisecond }
	lspCommand = func(_ tools.Bash, ctx context.Context, cwd, command string) *exec.Cmd {
		asked = append(asked, command)
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLSPFakeServer$") // #nosec G204 -- the test binary
		if ts := os.Getenv("ABHED_FAKE_LSP_TS"); ts != "" {
			// As the real script does for typescript-language-server: name the TypeScript beside it.
			if ts == "1" {
				ts = "/opt/ts/lib/tsserver.js"
			}
			c = exec.CommandContext(ctx, "/bin/sh", "-c", `echo "`+lsprelay.TSServerMarker+ts+`" >&2; exec "$0" -test.run='^TestLSPFakeServer$'`, os.Args[0]) // #nosec G204 -- the test binary
		}
		if os.Getenv("ABHED_FAKE_LSP_EXIT") != "" {
			c = exec.CommandContext(ctx, "/bin/sh", "-c", "exit 1") // #nosec G204 -- a test's own script
		}
		c.Env = append(append(os.Environ(), "ABHED_FAKE_LSP=1", "ABHED_FAKE_LSP_ROOT="+root), env...)
		c.Dir = cwd
		return c
	}
	t.Cleanup(func() { lspCommand, lspTier, lspBackoff = oldCmd, oldTier, oldBackoff })
	return &asked
}

func lspMessages(msgs []rpcMessage) []string {
	var out []string
	for _, m := range msgs {
		if m.Method == "_abhed/lsp/message" {
			out = append(out, string(m.Params))
		}
	}
	return out
}

// §7.9: the engine starts and initializes the server as the person's judged
// call; Studio's requests pass; an external showDocument is answered by the
// engine, an outside file's diagnostics dropped, an unsafe command and a
// file outside the workspace refused; stop ends it, recorded with its counts.
func TestStudioRunsALanguageServer(t *testing.T) {
	r := newStudioRig(t, "")
	asked := fakeLSP(t, r.ws)
	id := r.open()
	from := r.cl.mark()
	var res struct {
		LSPID        string         `json:"lspId"`
		Capabilities map[string]any `json:"capabilities"`
	}
	r.cl.ok("_abhed/lsp/start", map[string]any{"sessionId": id, "language": "go"}, &res)
	if res.LSPID == "" || res.Capabilities["hoverProvider"] != true || res.Capabilities["fakeOptions"] != false {
		t.Fatalf("start %+v", res)
	}
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "exec env GOTOOLCHAIN=local GOFLAGS= GOENV=off gopls") {
		t.Fatalf("command %v", *asked)
	}
	r.cl.waitFor(from, "the engine's showDocument answer", func(m rpcMessage) bool {
		return m.Method == "_abhed/lsp/message" && strings.Contains(string(m.Params), "showDocument answered map[success:false]")
	})
	r.cl.waitFor(from, "the inside diagnostics", func(m rpcMessage) bool {
		return m.Method == "_abhed/lsp/message" && strings.Contains(string(m.Params), r.ws+"/a.go")
	})
	send := func(msg string) map[string]any {
		var out map[string]any
		r.cl.ok("_abhed/lsp/send", map[string]any{"lspId": res.LSPID, "message": json.RawMessage(msg)}, &out)
		return out
	}
	send(`{"jsonrpc":"2.0","id":7,"method":"textDocument/hover","params":{"textDocument":{"uri":"file://` + r.ws + `/a.go"},"position":{"line":0,"character":0}}}`)
	r.cl.waitFor(from, "hover", func(m rpcMessage) bool {
		return m.Method == "_abhed/lsp/message" && strings.Contains(string(m.Params), "fake hover")
	})
	if out := send(`{"jsonrpc":"2.0","id":8,"method":"workspace/executeCommand","params":{"command":"gopls.run_go_work_command"}}`); out["refused"] == nil {
		t.Fatalf("an unsafe command was passed: %v", out)
	}
	r.cl.waitFor(from, "the command's refusal", func(m rpcMessage) bool {
		return m.Method == "_abhed/lsp/message" && strings.Contains(string(m.Params), `"id":8`) && strings.Contains(string(m.Params), "error")
	})
	if out := send(`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///etc/passwd","text":""}}}`); out["refused"] == nil {
		t.Fatalf("an outside file was passed: %v", out)
	}
	r.cl.ok("_abhed/lsp/stop", map[string]any{"lspId": res.LSPID}, nil)
	r.cl.waitFor(from, "ended", func(m rpcMessage) bool {
		return m.Method == "_abhed/lsp/status" && strings.Contains(string(m.Params), `"ended"`)
	})
	for _, m := range lspMessages(r.cl.since(from)) {
		if strings.Contains(m, "/etc/passwd") || strings.Contains(m, "evil.example") || strings.Contains(m, "executeCommand reached") {
			t.Fatalf("Studio got %s", m)
		}
	}
	started, _ := r.recorded(id, agent.EvLSPStarted)
	stopped, _ := r.recorded(id, agent.EvLSPStopped)
	if len(started) != 1 || started[0]["language"] != "go" || started[0]["command"] != "gopls" {
		t.Fatalf("lsp.started %v", started)
	}
	if len(stopped) != 1 || stopped[0]["reason"] != "stopped" || stopped[0]["requests"] != float64(1) || stopped[0]["refused"] != float64(2) {
		t.Fatalf("lsp.stopped %v", stopped)
	}
}

// A crash restarts the server, judged and initialized again, and Studio is
// told so with its capabilities; a deny rule on the server refuses the start.
func TestStudioLanguageServerRestartsAndIsJudged(t *testing.T) {
	r := newStudioRig(t, "")
	fakeLSP(t, r.ws, "ABHED_FAKE_LSP_CRASH="+r.ws+"/../crashed-once")
	id := r.open()
	from := r.cl.mark()
	var res struct {
		LSPID string `json:"lspId"`
	}
	r.cl.ok("_abhed/lsp/start", map[string]any{"sessionId": id, "language": "go"}, &res)
	r.cl.waitFor(from, "restarted", func(m rpcMessage) bool {
		return m.Method == "_abhed/lsp/status" && strings.Contains(string(m.Params), `"restarted"`) && strings.Contains(string(m.Params), "hoverProvider")
	})
	r.cl.ok("_abhed/lsp/stop", map[string]any{"lspId": res.LSPID}, nil)
	r.cl.waitFor(from, "ended", func(m rpcMessage) bool {
		return m.Method == "_abhed/lsp/status" && strings.Contains(string(m.Params), `"ended"`)
	})
	stopped, _ := r.recorded(id, agent.EvLSPStopped)
	if len(stopped) != 2 || stopped[0]["reason"] != "crashed" || stopped[1]["reason"] != "stopped" {
		t.Fatalf("lsp.stopped %v", stopped)
	}

	d := newStudioRig(t, `,"permissions":{"deny":["bash(gopls*)"]}`)
	asked := fakeLSP(t, d.ws)
	did := d.open()
	d.cl.refused(errPolicy, "_abhed/lsp/start", map[string]any{"sessionId": did, "language": "go"})
	if len(*asked) != 0 {
		t.Fatalf("a refused server started: %v", *asked)
	}
	d.cl.refused(errParams, "_abhed/lsp/start", map[string]any{"sessionId": did, "language": "cobol"})
}

// Studio's messages reach the server in the order they were sent, however
// fast they come.
func TestStudioLanguageServerKeepsOrder(t *testing.T) {
	r := newStudioRig(t, "")
	fakeLSP(t, r.ws)
	id := r.open()
	var res struct {
		LSPID string `json:"lspId"`
	}
	r.cl.ok("_abhed/lsp/start", map[string]any{"sessionId": id, "language": "go"}, &res)
	from := r.cl.mark()
	const n = 60
	for i := 1; i <= n; i++ {
		params, _ := json.Marshal(map[string]any{"lspId": res.LSPID, "message": map[string]any{"jsonrpc": "2.0", "method": "textDocument/didChange",
			"params": map[string]any{"textDocument": map[string]any{"uri": "file://" + r.ws + "/a.go", "version": i}, "contentChanges": []any{}}}})
		r.cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprint(1000 + i)), Method: "_abhed/lsp/send", Params: params})
	}
	r.cl.waitFor(from, "the last version", func(m rpcMessage) bool {
		return m.Method == "_abhed/lsp/message" && strings.Contains(string(m.Params), fmt.Sprintf("version %d\"", n))
	})
	next := 1
	for _, m := range lspMessages(r.cl.since(from)) {
		if i := strings.Index(m, "version "); i >= 0 {
			var v int
			fmt.Sscanf(m[i+len("version "):], "%d", &v)
			if v != next {
				t.Fatalf("version %d arrived where %d was due", v, next)
			}
			next++
		}
	}
	r.cl.ok("_abhed/lsp/stop", map[string]any{"lspId": res.LSPID}, nil)
}

// A missing server Abhed installs points at the toolchain route; another
// server, at an image or sandbox.language_servers.
func TestLSPMissingStepsPointAtTheInstall(t *testing.T) {
	for lang, prog := range map[string]string{"go": "gopls", "python": "basedpyright-langserver", "typescript": "typescript-language-server", "rust": "rust-analyzer"} {
		steps := strings.Join(lspMissingSteps(lang, prog), " | ")
		want := "abhed toolchain install -lsp "
		if !strings.Contains(steps, want) || !strings.Contains(steps, "lsp true") {
			t.Errorf("%s: %s", lang, steps)
		}
		if lang == "typescript" && !strings.Contains(steps, "-lsp node") {
			t.Errorf("typescript's server comes with node: %s", steps)
		}
	}
	if steps := strings.Join(lspMissingSteps("go", "my-gopls-fork"), " | "); strings.Contains(steps, "toolchain install") {
		t.Errorf("a server Abhed does not install pointed at the install: %s", steps)
	}
}

// The engine's settings reach the server, not Studio's: rust-analyzer gets
// build scripts, proc macros and check on save off, in its initialize and in
// every configuration answer; its own methods that run cargo are refused;
// typescript-language-server is given the TypeScript beside it.
func TestStudioLanguageServerSafeSettings(t *testing.T) {
	// The rules judge would refuse a server not on this host's PATH; the fake stands in.
	r := newStudioRig(t, `,"monitor":{"enabled":false}`)
	fakeLSP(t, r.ws, "ABHED_FAKE_LSP_CONFIG=1")
	id := r.open()
	from := r.cl.mark()
	var res struct {
		LSPID        string         `json:"lspId"`
		Capabilities map[string]any `json:"capabilities"`
	}
	r.cl.ok("_abhed/lsp/start", map[string]any{"sessionId": id, "language": "rust"}, &res)
	opts, _ := json.Marshal(res.Capabilities["initOptions"])
	if !strings.Contains(string(opts), `"procMacro":{"enable":false}`) || !strings.Contains(string(opts), `"buildScripts":{"enable":false}`) || !strings.Contains(string(opts), `"checkOnSave":false`) {
		t.Fatalf("initializationOptions %s", opts)
	}
	r.cl.waitFor(from, "the configuration request", func(m rpcMessage) bool {
		return m.Method == "_abhed/lsp/message" && strings.Contains(string(m.Params), "workspace/configuration")
	})
	send := func(msg string) map[string]any {
		var out map[string]any
		r.cl.ok("_abhed/lsp/send", map[string]any{"lspId": res.LSPID, "message": json.RawMessage(msg)}, &out)
		return out
	}
	send(`{"jsonrpc":"2.0","id":"c1","result":[{"procMacro":{"enable":true},"cargo":{"buildScripts":{"enable":true}}}]}`)
	r.cl.waitFor(from, "the engine's configuration", func(m rpcMessage) bool {
		return m.Method == "_abhed/lsp/message" && strings.Contains(string(m.Params), `configuration answered [{\"cargo\":{\"autoreload\":false,\"buildScripts\":{\"enable\":false}}`)
	})
	for _, method := range []string{"experimental/runTest", "rust-analyzer/runFlycheck", "rust-analyzer/rebuildProcMacros", "rust-analyzer/reloadWorkspace"} {
		if out := send(`{"jsonrpc":"2.0","id":9,"method":"` + method + `","params":{}}`); out["refused"] == nil {
			t.Fatalf("%s was passed", method)
		}
	}
	r.cl.ok("_abhed/lsp/stop", map[string]any{"lspId": res.LSPID}, nil)

	ts := newStudioRig(t, `,"monitor":{"enabled":false}`)
	asked := fakeLSP(t, ts.ws, "ABHED_FAKE_LSP_TS=1")
	t.Setenv("ABHED_FAKE_LSP_TS", "1")
	tid := ts.open()
	var tres struct {
		LSPID        string         `json:"lspId"`
		Capabilities map[string]any `json:"capabilities"`
	}
	ts.cl.ok("_abhed/lsp/start", map[string]any{"sessionId": tid, "language": "typescript"}, &tres)
	topts, _ := json.Marshal(tres.Capabilities["initOptions"])
	if !strings.Contains(string(topts), `"tsserver":{"path":"/opt/ts/lib/tsserver.js"}`) {
		t.Fatalf("typescript initializationOptions %s", topts)
	}
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "lib/node_modules/typescript-language-server") {
		t.Fatalf("command %v", *asked)
	}
	ts.cl.ok("_abhed/lsp/stop", map[string]any{"lspId": tres.LSPID}, nil)
}

// A supervisor's ask on a server start reaches the person as a permission
// request marked lsp; Allow starts it, Deny or no answer refuses it.
func TestStudioLanguageServerSupervisorAsks(t *testing.T) {
	for _, c := range []struct {
		answer string
		ok     bool
	}{{"once:", true}, {"reject:", false}, {"", false}} {
		r := newStudioRig(t, "")
		asked := fakeLSP(t, r.ws)
		id := r.open()
		withSupervisor(t, r, id, &monitor.Guard{Monitor: judging(policy.Ask, "unknown-program", nil)})
		var metas []map[string]any
		r.cl.answering(func(method string, params json.RawMessage) any {
			if method != "session/request_permission" {
				return map[string]any{}
			}
			var p struct {
				ToolCall struct {
					ToolCallID string                    `json:"toolCallId"`
					Meta       map[string]map[string]any `json:"_meta"`
				} `json:"toolCall"`
			}
			_ = json.Unmarshal(params, &p)
			metas = append(metas, p.ToolCall.Meta[acpMetaKey])
			if c.answer == "" {
				return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
			}
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": c.answer + p.ToolCall.ToolCallID}}
		})
		params := map[string]any{"sessionId": id, "language": "go"}
		if c.ok {
			var res struct {
				LSPID string `json:"lspId"`
			}
			r.cl.ok("_abhed/lsp/start", params, &res)
			r.cl.ok("_abhed/lsp/stop", map[string]any{"lspId": res.LSPID}, nil)
		} else {
			r.cl.refused(errPolicy, "_abhed/lsp/start", params)
		}
		if len(metas) != 1 || metas[0]["lsp"] != true || metas[0]["step"] != "monitor" {
			t.Fatalf("%q: permission requests %v", c.answer, metas)
		}
		if (len(*asked) == 1) != c.ok {
			t.Fatalf("%q: servers started %v", c.answer, *asked)
		}
	}
}

// Between a crash and the restart a send is refused as restarting, not lost;
// a second start waits for the first to initialize; a session's close ends
// its servers as closed.
func TestStudioLanguageServerRestartWindowAndClose(t *testing.T) {
	r := newStudioRig(t, "")
	fakeLSP(t, r.ws, "ABHED_FAKE_LSP_CRASH="+r.ws+"/../crashed-once-2")
	lspBackoff = func(int) time.Duration { return 1500 * time.Millisecond }
	id := r.open()
	from := r.cl.mark()
	results := make(chan map[string]any, 2)
	for range 2 {
		go func() {
			m := r.cl.call("_abhed/lsp/start", map[string]any{"sessionId": id, "language": "go"})
			var res map[string]any
			_ = json.Unmarshal(m.Result, &res)
			results <- res
		}()
	}
	var lspID string
	for range 2 {
		res := <-results
		caps, _ := res["capabilities"].(map[string]any)
		if caps["hoverProvider"] != true {
			t.Fatalf("a start answered without capabilities: %v", res)
		}
		lspID, _ = res["lspId"].(string)
	}
	busy := false
	for deadline := time.Now().Add(1200 * time.Millisecond); time.Now().Before(deadline) && !busy; time.Sleep(50 * time.Millisecond) {
		m := r.cl.call("_abhed/lsp/send", map[string]any{"lspId": lspID, "message": map[string]any{"jsonrpc": "2.0", "method": "textDocument/didChange",
			"params": map[string]any{"textDocument": map[string]any{"uri": "file://" + r.ws + "/a.go", "version": 1}, "contentChanges": []any{}}}})
		busy = m.Error != nil && m.Error.Code == errBusy && strings.Contains(m.Error.Message, "restarting")
	}
	if !busy {
		t.Fatal("a send while restarting was not refused as restarting")
	}
	r.cl.waitFor(from, "restarted", func(m rpcMessage) bool {
		return m.Method == "_abhed/lsp/status" && strings.Contains(string(m.Params), `"restarted"`)
	})
	r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
	stopped, _ := r.recorded(id, agent.EvLSPStopped)
	if len(stopped) != 2 || stopped[1]["reason"] != "closed" {
		t.Fatalf("lsp.stopped %v", stopped)
	}
}

// typescript-language-server is never initialized without the pin: no name
// in time, a start that exits first, or a name under the workspace, each
// end the start as server-missing, without a long wait for the second.
func TestStudioTypeScriptStartFailsClosed(t *testing.T) {
	old := lspTSWait
	lspTSWait = 400 * time.Millisecond
	t.Cleanup(func() { lspTSWait = old })
	for _, c := range []struct {
		name string
		env  []string
		want string
	}{
		{"no name", nil, "TypeScript"},
		{"exits first", []string{"ABHED_FAKE_LSP_EXIT=1"}, "TypeScript"},
		{"in the workspace", []string{"WS"}, "where a command could have put it"},
	} {
		r := newStudioRig(t, `,"monitor":{"enabled":false}`)
		env := c.env
		if len(env) == 1 && env[0] == "WS" {
			env = []string{"ABHED_FAKE_LSP_TS=" + r.ws + "/node_modules/typescript/lib/tsserver.js"}
		}
		for _, e := range env {
			k, v, _ := strings.Cut(e, "=")
			t.Setenv(k, v)
		}
		fakeLSP(t, r.ws, env...)
		id := r.open()
		began := time.Now()
		m := r.cl.call("_abhed/lsp/start", map[string]any{"sessionId": id, "language": "typescript"})
		if m.Error == nil || !strings.Contains(m.Error.Message, c.want) || !strings.Contains(fmt.Sprint(m.Error.Data), "server-missing") {
			t.Fatalf("%s: %+v %s", c.name, m.Error, m.Result)
		}
		if c.name == "exits first" && time.Since(began) > 5*time.Second {
			t.Fatalf("a start that exited took %v to report", time.Since(began))
		}
		for _, e := range env {
			k, _, _ := strings.Cut(e, "=")
			_ = os.Unsetenv(k)
		}
	}
}

// A workspace rust-analyzer.toml overrides the engine's settings: the start
// is put to the person, told so; Allow starts it, Deny or no answer does
// not. Without one, nobody is asked.
func TestStudioRustAnalyzerConfigAsksThePerson(t *testing.T) {
	for _, c := range []struct {
		answer string
		ok     bool
	}{{"once:", true}, {"reject:", false}, {"", false}} {
		r := newStudioRig(t, `,"monitor":{"enabled":false}`)
		asked := fakeLSP(t, r.ws)
		r.write("crates/a/rust-analyzer.toml", "[cargo.buildScripts]\nenable = true\n")
		id := r.open()
		var reasons []string
		r.cl.answering(func(method string, params json.RawMessage) any {
			if method != "session/request_permission" {
				return map[string]any{}
			}
			var p struct {
				ToolCall struct {
					ToolCallID string                    `json:"toolCallId"`
					Meta       map[string]map[string]any `json:"_meta"`
				} `json:"toolCall"`
			}
			_ = json.Unmarshal(params, &p)
			reasons = append(reasons, fmt.Sprint(p.ToolCall.Meta[acpMetaKey]["reason"], " step=", p.ToolCall.Meta[acpMetaKey]["step"]))
			if c.answer == "" {
				return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
			}
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": c.answer + p.ToolCall.ToolCallID}}
		})
		params := map[string]any{"sessionId": id, "language": "rust"}
		if c.ok {
			var res struct {
				LSPID string `json:"lspId"`
			}
			r.cl.ok("_abhed/lsp/start", params, &res)
			r.cl.ok("_abhed/lsp/stop", map[string]any{"lspId": res.LSPID}, nil)
		} else {
			r.cl.refused(errPolicy, "_abhed/lsp/start", params)
		}
		if len(reasons) != 1 || !strings.Contains(reasons[0], "crates/a/rust-analyzer.toml") || !strings.Contains(reasons[0], "build scripts") || !strings.Contains(reasons[0], "step=workspace") {
			t.Fatalf("%q: asked %v", c.answer, reasons)
		}
		if (len(*asked) == 1) != c.ok {
			t.Fatalf("%q: started %v", c.answer, *asked)
		}
		reqs, _ := r.recorded(id, agent.EvActionRequested)
		if len(reqs) == 0 || !strings.Contains(fmt.Sprint(reqs[len(reqs)-1]["reason"]), "rust-analyzer.toml") {
			t.Fatalf("not recorded with its reason: %v", reqs)
		}
	}
	r := newStudioRig(t, `,"monitor":{"enabled":false}`)
	asked := fakeLSP(t, r.ws)
	id := r.open()
	var res struct {
		LSPID string `json:"lspId"`
	}
	r.cl.ok("_abhed/lsp/start", map[string]any{"sessionId": id, "language": "rust"}, &res)
	if len(*asked) != 1 {
		t.Fatal("a rust start with no rust-analyzer.toml did not start")
	}
	r.cl.ok("_abhed/lsp/stop", map[string]any{"lspId": res.LSPID}, nil)
}

// When the supervisor asks about a rust-analyzer start too, the person gets
// one question carrying both reasons, and both are recorded.
func TestStudioRustAnalyzerConfigAndSupervisorAskOnce(t *testing.T) {
	r := newStudioRig(t, "")
	asked := fakeLSP(t, r.ws)
	r.write("rust-analyzer.toml", "[procMacro]\nenable = true\n")
	id := r.open()
	withSupervisor(t, r, id, &monitor.Guard{Monitor: judging(policy.Ask, "unknown-program", nil)})
	var reasons []string
	r.cl.answering(func(method string, params json.RawMessage) any {
		if method != "session/request_permission" {
			return map[string]any{}
		}
		var p struct {
			ToolCall struct {
				ToolCallID string                    `json:"toolCallId"`
				Meta       map[string]map[string]any `json:"_meta"`
			} `json:"toolCall"`
		}
		_ = json.Unmarshal(params, &p)
		reasons = append(reasons, fmt.Sprint(p.ToolCall.Meta[acpMetaKey]["reason"], " step=", p.ToolCall.Meta[acpMetaKey]["step"]))
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "once:" + p.ToolCall.ToolCallID}}
	})
	var res struct {
		LSPID string `json:"lspId"`
	}
	r.cl.ok("_abhed/lsp/start", map[string]any{"sessionId": id, "language": "rust"}, &res)
	r.cl.ok("_abhed/lsp/stop", map[string]any{"lspId": res.LSPID}, nil)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "the judge's reason") || !strings.Contains(reasons[0], "rust-analyzer.toml") || !strings.Contains(reasons[0], "step=monitor") {
		t.Fatalf("asked %v", reasons)
	}
	if len(*asked) != 1 {
		t.Fatalf("started %v", *asked)
	}
	reqs, _ := r.recorded(id, agent.EvActionRequested)
	last := fmt.Sprint(reqs[len(reqs)-1]["reason"])
	if !strings.Contains(last, "the judge's reason") || !strings.Contains(last, "rust-analyzer.toml") {
		t.Fatalf("recorded reason %q", last)
	}
}

// A rust-analyzer.toml that appears while rust-analyzer runs stops it, as
// workspace-config-changed, recorded, so the next start asks again: found
// by the watch, written by the agent, or reported by Studio.
func TestStudioRustAnalyzerStopsOnConfigChange(t *testing.T) {
	oldEvery := lsprelay.RustAnalyzerWatchEvery
	lsprelay.RustAnalyzerWatchEvery = 50 * time.Millisecond
	t.Cleanup(func() { lsprelay.RustAnalyzerWatchEvery = oldEvery })
	start := func(r *studioRig, id string) string {
		var res struct {
			LSPID string `json:"lspId"`
		}
		r.cl.ok("_abhed/lsp/start", map[string]any{"sessionId": id, "language": "rust"}, &res)
		return res.LSPID
	}
	ended := func(r *studioRig, id string, from int) {
		r.cl.waitFor(from, "ended as workspace-config-changed", func(m rpcMessage) bool {
			return m.Method == "_abhed/lsp/status" && strings.Contains(string(m.Params), `"ended"`) && strings.Contains(string(m.Params), "workspace-config-changed")
		})
		var stopped []map[string]any
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if stopped, _ = r.recorded(id, agent.EvLSPStopped); len(stopped) > 0 {
				break
			}
		}
		if len(stopped) == 0 || stopped[len(stopped)-1]["reason"] != "workspace-config-changed" {
			t.Fatalf("lsp.stopped %v", stopped)
		}
	}

	// The watch.
	r := newStudioRig(t, `,"monitor":{"enabled":false}`)
	fakeLSP(t, r.ws)
	id := r.open()
	from := r.cl.mark()
	start(r, id)
	r.write("rust-analyzer.toml", "[cargo.buildScripts]\nenable = true\n")
	ended(r, id, from)

	// The agent's write.
	r = newStudioRig(t, `,"monitor":{"enabled":false}`)
	fakeLSP(t, r.ws)
	id = r.open()
	from = r.cl.mark()
	start(r, id)
	args, _ := json.Marshal(map[string]any{"path": r.ws + "/crates/b/rust-analyzer.toml", "content": "x"})
	payload, _ := json.Marshal(agent.ActionRequested{CallID: "w1", Tool: "write", Args: args})
	r.cl.conn.lspNoteAction(r.cl.conn.session(id), abhed.Event{Type: agent.EvActionRequested, Payload: payload})
	ended(r, id, from)

	// Studio's report of one.
	r = newStudioRig(t, `,"monitor":{"enabled":false}`)
	fakeLSP(t, r.ws)
	id = r.open()
	from = r.cl.mark()
	lspID := start(r, id)
	var out map[string]any
	r.cl.ok("_abhed/lsp/send", map[string]any{"lspId": lspID, "message": json.RawMessage(`{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file://` + r.ws + `/crates/c/.rust-analyzer.toml"}}}`)}, &out)
	if out["refused"] == nil {
		t.Fatalf("the report was passed to the server: %v", out)
	}
	ended(r, id, from)
}

// The TypeScript Abhed's toolchain installs sits in its cache under the home
// folder: Studio takes it there, and refuses one elsewhere under home.
func TestStudioTypeScriptFromTheToolchainCache(t *testing.T) {
	old := lspTSWait
	lspTSWait = 2 * time.Second
	t.Cleanup(func() { lspTSWait = old })
	for _, c := range []struct {
		name, ts string
		ok       bool
	}{
		{"in the cache", ".cache/abhed/toolchains/darwin-arm64/node/22.20.0/lib/node_modules/typescript/lib/tsserver.js", true},
		{"elsewhere under home", "npm/lib/node_modules/typescript/lib/tsserver.js", false},
	} {
		r := newStudioRig(t, `,"monitor":{"enabled":false}`)
		home, _ := os.UserHomeDir()
		oldRoot := lspToolchainRoot
		lspToolchainRoot = func(config.Config) string { return home + "/.cache/abhed/toolchains" }
		ts := home + "/" + c.ts
		t.Setenv("ABHED_FAKE_LSP_TS", ts)
		fakeLSP(t, r.ws, "ABHED_FAKE_LSP_TS="+ts)
		id := r.open()
		m := r.cl.call("_abhed/lsp/start", map[string]any{"sessionId": id, "language": "typescript"})
		if (m.Error == nil) != c.ok {
			t.Fatalf("%s: %+v %s", c.name, m.Error, m.Result)
		}
		if c.ok {
			var res struct {
				LSPID string `json:"lspId"`
			}
			_ = json.Unmarshal(m.Result, &res)
			r.cl.ok("_abhed/lsp/stop", map[string]any{"lspId": res.LSPID}, nil)
		}
		lspToolchainRoot = oldRoot
	}
}

// A stop that comes while the start is being decided holds: an agent's
// write of a rust-analyzer.toml during the question, or the person's own
// stop, and no server runs; Studio is told it ended, and the end recorded.
func TestStudioRustAnalyzerStopDuringTheQuestion(t *testing.T) {
	for _, c := range []struct{ name, reason string }{
		{"agent write", "workspace-config-changed"},
		{"person's stop", "stopped"},
	} {
		r := newStudioRig(t, `,"monitor":{"enabled":false}`)
		asked := fakeLSP(t, r.ws)
		r.write("crates/a/rust-analyzer.toml", "[cargo]\n")
		id := r.open()
		from := r.cl.mark()
		r.cl.answering(func(method string, params json.RawMessage) any {
			if method != "session/request_permission" {
				return map[string]any{}
			}
			var p struct {
				ToolCall struct {
					ToolCallID string `json:"toolCallId"`
				} `json:"toolCall"`
			}
			_ = json.Unmarshal(params, &p)
			if c.reason == "stopped" {
				r.cl.conn.lspMu.Lock()
				var mine []*acpLSP
				for _, l := range r.cl.conn.lsps {
					mine = append(mine, l)
				}
				r.cl.conn.lspMu.Unlock()
				for _, l := range mine {
					l.stop("stopped")
				}
			} else {
				r.write("crates/a/rust-analyzer.toml", "[cargo.buildScripts]\nenable = true\n")
				args, _ := json.Marshal(map[string]any{"path": r.ws + "/crates/a/rust-analyzer.toml", "content": "x"})
				payload, _ := json.Marshal(agent.ActionRequested{CallID: "w1", Tool: "write", Args: args})
				r.cl.conn.lspNoteAction(r.cl.conn.session(id), abhed.Event{Type: agent.EvActionRequested, Payload: payload})
			}
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "once:" + p.ToolCall.ToolCallID}}
		})
		m := r.cl.call("_abhed/lsp/start", map[string]any{"sessionId": id, "language": "rust"})
		if m.Error == nil {
			t.Fatalf("%s: the start went ahead: %s", c.name, m.Result)
		}
		if len(*asked) != 0 {
			t.Fatalf("%s: a server ran: %v", c.name, *asked)
		}
		r.cl.waitFor(from, "ended", func(m rpcMessage) bool {
			return m.Method == "_abhed/lsp/status" && strings.Contains(string(m.Params), `"ended"`) && strings.Contains(string(m.Params), c.reason)
		})
		stopped, _ := r.recorded(id, agent.EvLSPStopped)
		if len(stopped) != 1 || stopped[0]["reason"] != c.reason {
			t.Fatalf("%s: lsp.stopped %v", c.name, stopped)
		}
		r.cl.conn.lspMu.Lock()
		left := len(r.cl.conn.lsps)
		r.cl.conn.lspMu.Unlock()
		if left != 0 {
			t.Fatalf("%s: %d servers still listed", c.name, left)
		}
	}
}

// stopRig gives the helpers for a stop that comes as the server's process
// is being started: a wrapped lspCommand that stops the session's
// rust-analyzer on its call-th use, a wait for Studio's ended, and the
// count of servers listed.
func stopRig() (func(r *studioRig, id string, call int), func(r *studioRig, from int), func(r *studioRig) int) {
	stopWhenStarting := func(r *studioRig, id string, call int) {
		inner := lspCommand
		n := 0
		lspCommand = func(b tools.Bash, ctx context.Context, cwd, command string) *exec.Cmd {
			n++
			if n == call {
				r.cl.conn.lspWorkspaceConfig(id)
			}
			return inner(b, ctx, cwd, command)
		}
	}
	ended := func(r *studioRig, from int) {
		r.cl.waitFor(from, "ended as workspace-config-changed", func(m rpcMessage) bool {
			return m.Method == "_abhed/lsp/status" && strings.Contains(string(m.Params), `"ended"`) && strings.Contains(string(m.Params), "workspace-config-changed")
		})
	}
	listed := func(r *studioRig) int {
		r.cl.conn.lspMu.Lock()
		defer r.cl.conn.lspMu.Unlock()
		return len(r.cl.conn.lsps)
	}
	return stopWhenStarting, ended, listed
}

// A stop that comes as the process is being started, after the check made
// before it, is caught where the process is set up: it is ended at once and
// never initialized, recorded once with the stop's reason, and Studio is
// told it ended. The same holds for a restart a stop overtakes.
func TestStudioRustAnalyzerStopCaughtUnderTheLock(t *testing.T) {
	stopWhenStarting, ended, listed := stopRig()
	r := newStudioRig(t, `,"monitor":{"enabled":false}`)
	fakeLSP(t, r.ws)
	id := r.open()
	from := r.cl.mark()
	stopWhenStarting(r, id, 1)
	if m := r.cl.call("_abhed/lsp/start", map[string]any{"sessionId": id, "language": "rust"}); m.Error == nil {
		t.Fatalf("the start went ahead: %s", m.Result)
	}
	ended(r, from)
	stopped, _ := r.recorded(id, agent.EvLSPStopped)
	if len(stopped) != 1 || stopped[0]["reason"] != "workspace-config-changed" {
		t.Fatalf("lsp.stopped %v", stopped)
	}
	for _, m := range r.cl.since(from) {
		if m.Method == "_abhed/lsp/status" && strings.Contains(string(m.Params), `"restarted"`) {
			t.Fatalf("it was restarted: %s", m.Params)
		}
	}
	if n := listed(r); n != 0 {
		t.Fatalf("%d servers still listed", n)
	}
}

// A restart after a crash that a stop overtakes ends with the stop's
// reason, not restarted and not as a crash.
func TestStudioRustAnalyzerRestartOvertakenByAStop(t *testing.T) {
	stopWhenStarting, ended, listed := stopRig()
	r := newStudioRig(t, `,"monitor":{"enabled":false}`)
	fakeLSP(t, r.ws, "ABHED_FAKE_LSP_CRASH="+r.ws+"/../crashed-before-stop")
	id := r.open()
	from := r.cl.mark()
	stopWhenStarting(r, id, 2)
	var res struct {
		LSPID string `json:"lspId"`
	}
	r.cl.ok("_abhed/lsp/start", map[string]any{"sessionId": id, "language": "rust"}, &res)
	ended(r, from)
	var reasons []any
	var stopped []map[string]any
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		stopped, _ = r.recorded(id, agent.EvLSPStopped)
		if len(stopped) >= 2 {
			break
		}
	}
	for _, s := range stopped {
		reasons = append(reasons, s["reason"])
	}
	if len(stopped) != 2 || stopped[0]["reason"] != "crashed" || stopped[1]["reason"] != "workspace-config-changed" {
		t.Fatalf("lsp.stopped reasons %v", reasons)
	}
	for _, m := range r.cl.since(from) {
		if m.Method == "_abhed/lsp/status" && strings.Contains(string(m.Params), `"restarted"`) {
			t.Fatalf("it was restarted: %s", m.Params)
		}
	}
	if n := listed(r); n != 0 {
		t.Fatalf("%d servers still listed", n)
	}
}
