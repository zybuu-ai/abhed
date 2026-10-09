package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// layerBench is a workbench whose edition gives every session the layer
// that layer returns.
func layerBench(t *testing.T, layer func() (*PolicyLayer, error)) *workbench {
	return shellBenchOpts(t, nil, func(o *Options) {
		o.SessionPolicy = func(context.Context, SessionPolicyRequest) (*PolicyLayer, error) { return layer() }
	})
}

func ptyStart(t *testing.T, wb *workbench, req ptyStartRequest) (int, ptyStartResponse) {
	t.Helper()
	rec := wb.send("acme", "POST", "pty", req)
	var got ptyStartResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return rec.Code, got
}

// A workspace's deny or ask rule for commands makes the terminal judge each
// line, with the layer; a layer that cannot be read opens no terminal.
func TestShellTakesTheWorkspaceLayer(t *testing.T) {
	for name, l := range map[string]PolicyLayer{
		"deny": {Name: "workspace pay", Deny: []string{"bash(curl*)"}},
		"ask":  {Name: "workspace pay", Ask: []string{"bash(curl*)"}},
		"any":  {Name: "workspace pay", Deny: []string{"*(*secret*)"}},
	} {
		t.Run(name, func(t *testing.T) {
			wb := layerBench(t, func() (*PolicyLayer, error) { return &l, nil })
			code, got := ptyStart(t, wb, ptyStartRequest{Interactive: true})
			if code != http.StatusOK || got.Interactive || got.ID != "" || !strings.Contains(got.Lines, "your workspace's rules") {
				t.Fatalf("%d %+v", code, got)
			}
			if name != "deny" {
				return
			}
			// Each line is judged with the layer.
			code, got = ptyStart(t, wb, ptyStartRequest{Command: "curl https://evil.example/x"})
			if code != http.StatusOK || got.Denied == "" || !strings.Contains(got.Denied, "workspace pay") {
				t.Fatalf("a line the layer denies: %d %+v", code, got)
			}
		})
	}

	var mu sync.Mutex
	var fail error
	layer := &PolicyLayer{Name: "workspace pay", Allow: []string{"read(*)"}}
	wb := layerBench(t, func() (*PolicyLayer, error) {
		mu.Lock()
		defer mu.Unlock()
		return layer, fail
	})
	// No rule that screens commands: the shell stays interactive.
	start := wb.startShell()
	if start.Lines != "" || !start.Interactive {
		t.Fatalf("a layer with no command rules fell back to lines: %+v", start)
	}
	_, _ = wb.typeLines(start.ID, enter("exit")...)

	mu.Lock()
	fail = errors.New("the workspace store is down")
	mu.Unlock()
	if code, got := ptyStart(t, wb, ptyStartRequest{Interactive: true}); code != http.StatusServiceUnavailable || got.Interactive {
		t.Fatalf("a layer that cannot be read: %d %+v", code, got)
	}

	// Rules added since the session opened are not in its engine: no terminal
	// until it is opened again.
	mu.Lock()
	fail, layer = nil, &PolicyLayer{Name: "workspace pay", Deny: []string{"bash(curl*)"}}
	mu.Unlock()
	if code, got := ptyStart(t, wb, ptyStartRequest{Interactive: true}); code != http.StatusConflict || got.Interactive {
		t.Fatalf("a layer changed since the session opened: %d %+v", code, got)
	}
}
