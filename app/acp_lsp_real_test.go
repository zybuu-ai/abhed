package app

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A real gopls behind the relay, in the session's real sandbox. Opt in with
// ABHED_LSP_REAL=1 and gopls on PATH.
func TestStudioRunsGoplsForReal(t *testing.T) {
	if os.Getenv("ABHED_LSP_REAL") == "" {
		t.Skip("set ABHED_LSP_REAL=1, with gopls on PATH, to run gopls in the sandbox")
	}
	gopls, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls is not on PATH")
	}
	dir := filepath.Dir(gopls)
	r := newStudioRig(t, `,"sandbox":{"read_only_paths":["`+dir+`"],"language_servers":{"go":["`+gopls+`"]}}`)
	id := r.open()
	r.write("go.mod", "module realgo\n\ngo 1.24\n")
	src := "package main\n\n// Double returns twice n.\nfunc Double(n int) int { return 2 * n }\n\nfunc main() { _ = Double(2) }\n"
	r.write("main.go", src)
	var res struct {
		LSPID        string         `json:"lspId"`
		Capabilities map[string]any `json:"capabilities"`
	}
	r.cl.ok("_abhed/lsp/start", map[string]any{"sessionId": id, "language": "go"}, &res)
	if res.Capabilities["hoverProvider"] == nil {
		t.Fatalf("capabilities %v", res.Capabilities)
	}
	uri := "file://" + r.ws + "/main.go"
	send := func(v any) {
		b, _ := json.Marshal(v)
		var out map[string]any
		r.cl.ok("_abhed/lsp/send", map[string]any{"lspId": res.LSPID, "message": json.RawMessage(b)}, &out)
		if out["refused"] != nil {
			t.Fatalf("refused: %v", out)
		}
	}
	from := r.cl.mark()
	defer func() {
		if t.Failed() {
			for _, m := range r.cl.since(from) {
				t.Logf("%s %.400s", m.Method, m.Params)
			}
		}
	}()
	send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "go", "version": 1, "text": src}}})
	// The editor answers what the server asks, as Studio does: a progress token
	// gopls waits for before it loads the package.
	answered := map[string]bool{}
	answer := func() {
		for _, m := range r.cl.since(from) {
			if m.Method != "_abhed/lsp/message" {
				continue
			}
			var p struct {
				Message struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				} `json:"message"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if id := string(p.Message.ID); p.Message.Method != "" && id != "" && !answered[id] {
				answered[id] = true
				send(map[string]any{"jsonrpc": "2.0", "id": p.Message.ID, "result": nil})
			}
		}
	}
	r.cl.waitFor(from, "gopls asks for progress", func(m rpcMessage) bool {
		return m.Method == "_abhed/lsp/message" && strings.Contains(string(m.Params), "workDoneProgress/create")
	})
	answer()
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "textDocument/hover", "params": map[string]any{"textDocument": map[string]any{"uri": uri}, "position": map[string]int{"line": 5, "character": 19}}})
	// Answered outside waitFor: a reply sent from inside it would wait on itself.
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		answer()
		hovered := false
		for _, m := range r.cl.since(from) {
			hovered = hovered || m.Method == "_abhed/lsp/message" && strings.Contains(string(m.Params), `"id":2`) && strings.Contains(string(m.Params), "Double returns twice n")
		}
		if hovered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no hover from gopls")
		}
	}
	r.cl.ok("_abhed/lsp/stop", map[string]any{"lspId": res.LSPID}, nil)
}
