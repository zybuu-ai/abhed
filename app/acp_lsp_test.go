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

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/lsprelay"
	"github.com/zybuu-ai/abhed/internal/tools"
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
			_, hasOpts := params["initializationOptions"]
			send(map[string]any{"id": m["id"], "result": map[string]any{"capabilities": map[string]any{"hoverProvider": true, "fakeOptions": hasOpts}}})
		case method == "initialized":
			if f := os.Getenv("ABHED_FAKE_LSP_CRASH"); f != "" {
				if _, err := os.Stat(f); err != nil {
					_ = os.WriteFile(f, nil, 0o600)
					os.Exit(2)
				}
			}
			send(map[string]any{"id": "s1", "method": "window/showDocument", "params": map[string]any{"uri": "https://evil.example", "external": true}})
			send(map[string]any{"method": "textDocument/publishDiagnostics", "params": map[string]any{"uri": "file:///etc/passwd", "diagnostics": []any{}}})
			send(map[string]any{"method": "textDocument/publishDiagnostics", "params": map[string]any{"uri": "file://" + root + "/a.go", "diagnostics": []any{}}})
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
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "exec GOTOOLCHAIN=local gopls") {
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
