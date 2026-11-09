package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/lspdiag"
	"github.com/zybuu-ai/abhed/internal/lsprelay"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// TestDiagStubServer is a language server that reports one error per line
// holding BAD, run by TestEditsAreFollowedByDiagnostics.
func TestDiagStubServer(t *testing.T) {
	if os.Getenv("ABHED_DIAG_STUB") == "" {
		t.Skip("the stub server, run by the tests")
	}
	br := bufio.NewReader(os.Stdin)
	send := func(v any) {
		b, _ := json.Marshal(v)
		_ = lsprelay.Write(os.Stdout, b)
	}
	for {
		raw, err := lsprelay.Read(br)
		if err != nil {
			os.Exit(0)
		}
		var m struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				TextDocument struct {
					URI     string `json:"uri"`
					Version int    `json:"version"`
					Text    string `json:"text"`
				} `json:"textDocument"`
			} `json:"params"`
		}
		_ = json.Unmarshal(raw, &m)
		switch m.Method {
		case "initialize":
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"capabilities": map[string]any{}}})
		case "textDocument/didOpen":
			var diags []any
			for i, l := range strings.Split(m.Params.TextDocument.Text, "\n") {
				if strings.Contains(l, "BAD") {
					diags = append(diags, map[string]any{"range": map[string]any{"start": map[string]any{"line": i, "character": 0}},
						"severity": 1, "message": "stub: bad line", "source": "stub"})
				}
			}
			send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
				"params": map[string]any{"uri": m.Params.TextDocument.URI, "version": m.Params.TextDocument.Version, "diagnostics": diags}})
		case "shutdown":
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": nil})
		case "exit":
			os.Exit(0)
		}
	}
}

// A-16 through the editor protocol: the agent's write of a Go file is
// followed by the server's diagnostics in the result the model reads, and
// in the record; the server is started once, judged by policy as a bash
// call, and stopped with the session.
func TestEditsAreFollowedByDiagnostics(t *testing.T) {
	oldCmd, oldTier := lspdiag.Command, lspdiag.Tier
	var mu sync.Mutex
	var started []string
	lspdiag.Tier = func(tools.Bash) string { return "process" }
	lspdiag.Command = func(_ tools.Bash, ctx context.Context, cwd, command string) *exec.Cmd {
		mu.Lock()
		started = append(started, command)
		mu.Unlock()
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDiagStubServer$") // #nosec G204 -- the test binary
		c.Env = append(os.Environ(), "ABHED_DIAG_STUB=1")
		c.Dir = cwd
		return c
	}
	t.Cleanup(func() { lspdiag.Command, lspdiag.Tier = oldCmd, oldTier })

	r := newStudioRig(t, `,"permissions":{"allow":["write(**)","bash(/opt/stub/gopls)"]},"sandbox":{"language_servers":{"go":["/opt/stub/gopls"]}}`,
		callTool("w1", "write", map[string]any{"path": "a.go", "content": "package a\n\nvar x = BAD\n"}), say("written"))
	id := r.open()
	from := r.cl.mark()
	stop, ups := r.prompt(id, "write a.go")
	if stop != "end_turn" {
		t.Fatalf("stop %q", stop)
	}
	// Studio's card for the start names the language server, not a bash
	// command, and completes once the server is up, not at its end.
	var cardID string
	for _, u := range ups {
		if u["sessionUpdate"] == "tool_call" && strings.HasPrefix(fmt.Sprint(u["toolCallId"]), "diag-") {
			cardID = fmt.Sprint(u["toolCallId"])
			if u["title"] != "Language server: gopls" || u["kind"] != "other" {
				t.Fatalf("the start's card: %v", u)
			}
		}
	}
	if cardID == "" {
		t.Fatalf("no card for the start: %v", ups)
	}
	r.cl.waitFor(from, "the start's card completing", func(m rpcMessage) bool {
		var p struct {
			Update map[string]any `json:"update"`
		}
		_ = json.Unmarshal(m.Params, &p)
		return m.Method == "session/update" && p.Update["toolCallId"] == cardID && p.Update["status"] == "completed"
	})
	r.model.mu.Lock()
	sent := strings.Join(r.model.bodies, "\n")
	r.model.mu.Unlock()
	if !strings.Contains(sent, `[gopls diagnostics for a.go: 1 errors, 0 warnings, 1 in all]\na.go:3:1: error: stub: bad line (stub)`) {
		t.Fatalf("the model was not given the diagnostics:\n%s", sent)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(started) != 1 || !strings.Contains(started[0], "/opt/stub/gopls") {
		t.Fatalf("started %v", started)
	}
	var diag []agent.LSPDiagnostics
	for _, e := range r.events(id) {
		if e.Type == agent.EvLSPDiagnostics {
			var d agent.LSPDiagnostics
			_ = json.Unmarshal(e.Payload, &d)
			diag = append(diag, d)
		}
	}
	if len(diag) != 1 || diag[0].CallID != "w1" || diag[0].Errors != 1 || diag[0].Path != filepath.Join(r.ws, "a.go") {
		t.Fatalf("lsp.diagnostics %+v", diag)
	}
}
