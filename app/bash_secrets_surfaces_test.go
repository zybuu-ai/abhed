package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// surfaceSecret is a made-up value stored under SURFACE_TOKEN for these tests.
const surfaceSecret = "fake-surface-secret-5b91d7"

// secretWorkspace stores SURFACE_TOKEN, and writes a trusted workspace
// configuration whose model calls bash with it once, allowed by rule.
func secretWorkspace(t *testing.T) (string, func() string) {
	t.Helper()
	managedConfig(t, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// The default store, under the home's .abhed: a sandboxed surface refuses
	// one in a temp folder, where commands could replace it.
	t.Setenv("ABHED_SECRETS_FILE", "")
	dir := filepath.Join(os.Getenv("HOME"), ".abhed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(`{"SURFACE_TOKEN":"`+surfaceSecret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"command": `echo "k=[$SURFACE_TOKEN] n=${#SURFACE_TOKEN}"`,
		"description": "probe", "secrets": []string{"SURFACE_TOKEN"}})
	call, _ := json.Marshal(string(args))
	var n atomic.Int32
	var bodies atomic.Value
	bodies.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies.Store(bodies.Load().(string) + string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		if n.Add(1) == 1 {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash","arguments":`+string(call)+`}}]}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		} else {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"done"}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	cfg := `{"model": {"default": "fake", "providers": {"fake": {"type": "openai-compatible",
		"base_url": "` + srv.URL + `", "model": "m", "context_window": 8192}}},
		"permissions": {"allow": ["bash", "secret(SURFACE_TOKEN)"]}}`
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.TrustEnv, "1") // the test wrote this configuration
	return ws, func() string { return bodies.Load().(string) }
}

// wantSecretUsed checks the command ran with the stored value and that the
// value itself reached neither what the surface wrote nor the model.
func wantSecretUsed(t *testing.T, surface, out, sent string) {
	t.Helper()
	if !strings.Contains(out, fmt.Sprintf(`k=[[secret:SURFACE_TOKEN]] n=%d`, len(surfaceSecret))) {
		t.Errorf("%s: bash did not run with the stored secret:\n%s", surface, out)
	}
	if !strings.Contains(sent, "Secrets available by name") || !strings.Contains(sent, "SURFACE_TOKEN") {
		t.Errorf("%s: bash's description does not name the stored secret", surface)
	}
	if strings.Contains(out, surfaceSecret) || strings.Contains(sent, surfaceSecret) {
		t.Errorf("%s: the stored value left the session:\n%s", surface, out)
	}
}

// abhed rpc gives bash the stored secrets, as the CLI does.
func TestRPCBashUsesAStoredSecretByName(t *testing.T) {
	ws, sent := secretWorkspace(t)
	out := runRPC(t, ws,
		`{"id":"1","method":"start"}`,
		`{"id":"2","method":"prompt","prompt":"probe"}`,
		`{"id":"3","method":"quit"}`)
	wantSecretUsed(t, "rpc", out, sent())
}

// abhed acp gives bash the stored secrets, as the CLI does.
func TestACPBashUsesAStoredSecretByName(t *testing.T) {
	ws, sent := secretWorkspace(t)
	cl := newACPClient(t, func(_ string, params json.RawMessage) any { return chosen(params, "allow_once") })
	cl.request(1, "initialize", map[string]any{"protocolVersion": 1})
	created := cl.request(2, "session/new", map[string]any{"cwd": ws})
	if created.Error != nil {
		t.Fatalf("session/new failed: %s", created.Error.Message)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(created.Result, &sess)
	_, updates := cl.prompt(3, map[string]any{"sessionId": sess.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "probe"}}})
	raw, _ := json.Marshal(updates)
	// The editor's copy is JSON inside JSON; undo one level of escaping.
	wantSecretUsed(t, "acp", strings.ReplaceAll(string(raw), `\"`, `"`), sent())
}
