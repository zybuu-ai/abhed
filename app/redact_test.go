package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVaultValue is a made-up value, stored under FAKE_TOKEN for these tests.
const fakeVaultValue = "fake-entry-secret-3a9f07"

// fakeVault puts FAKE_TOKEN in the default store under a temporary home, with
// no managed configuration.
func fakeVault(t *testing.T) {
	t.Helper()
	managedConfig(t, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	dir := filepath.Join(os.Getenv("HOME"), ".abhed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(`{"FAKE_TOKEN":"`+fakeVaultValue+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// leakingModel reads creds.txt, asks to run a command naming the value and
// then says the value: every way a session could carry it out.
func leakingModel(t *testing.T) string {
	t.Helper()
	srv := scriptedEndpoint(t, func(dir string) []string {
		return []string{
			toolCall("read", map[string]string{"path": filepath.Join(dir, "creds.txt")}),
			toolCall("bash", map[string]string{"command": "echo " + fakeVaultValue, "description": "echo"}),
			`{"choices":[{"delta":{"content":"the token is ` + fakeVaultValue + `"}}]}`,
		}
	})
	return `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` +
		srv.URL + `","model":"m","context_window":8192}}}}`
}

// credsWorkspace is a workspace holding creds.txt and a config for model.
func credsWorkspace(t *testing.T, cfg string) string {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "creds.txt"), []byte("token="+fakeVaultValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return ws
}

// An editor on ACP is sent no stored value, in a session/update or in a
// permission request.
func TestACPSendsNoStoredSecret(t *testing.T) {
	fakeVault(t)
	ws := credsWorkspace(t, leakingModel(t))
	var mu sync.Mutex
	var asked []string
	cl := newACPClient(t, func(method string, params json.RawMessage) any {
		mu.Lock()
		asked = append(asked, string(params))
		mu.Unlock()
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "reject"}}
	})
	cl.request(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	created := cl.request(2, "session/new", map[string]any{"cwd": ws, "mcpServers": []any{}})
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(created.Result, &sess); err != nil || sess.SessionID == "" {
		t.Fatalf("session/new: %s %v", created.Result, created.Error)
	}
	// Every message the editor is sent during the prompt, whole.
	params, _ := json.Marshal(map[string]any{"sessionId": sess.SessionID,
		"prompt": []any{map[string]any{"type": "text", "text": "read creds.txt"}}})
	cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage("3"), Method: "session/prompt", Params: params})
	var wire strings.Builder
	deadline := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case m := <-cl.lines:
			b, _ := json.Marshal(m)
			wire.Write(append(b, '\n'))
			done = m.Method == "" && string(m.ID) == "3"
		case <-deadline:
			t.Fatalf("no response to session/prompt:\n%s", wire.String())
		}
	}
	mu.Lock()
	whole := wire.String() + strings.Join(asked, "\n")
	n := len(asked)
	mu.Unlock()
	if n != 1 || !strings.Contains(asked[0], "[secret:FAKE_TOKEN]") {
		t.Fatalf("want one permission request naming the secret, got %d:\n%s", n, strings.Join(asked, "\n"))
	}
	if !strings.Contains(wire.String(), `"sessionUpdate":"tool_call_update"`) || !strings.Contains(wire.String(), "[secret:FAKE_TOKEN]") {
		t.Fatalf("the read's output did not reach the editor redacted:\n%s", wire.String())
	}
	if strings.Contains(whole, fakeVaultValue) {
		t.Fatalf("the editor was sent the stored value:\n%s", whole)
	}
}

// abhed rpc writes no stored value, in an event or in the answer.
func TestRPCWritesNoStoredSecret(t *testing.T) {
	fakeVault(t)
	ws := credsWorkspace(t, leakingModel(t))
	out := runRPC(t, ws,
		`{"id":"1","method":"start"}`,
		`{"id":"2","method":"prompt","prompt":"read creds.txt"}`,
		`{"id":"3","method":"export"}`,
		`{"id":"4","method":"quit"}`)
	if !strings.Contains(out, `"type":"answer"`) || !strings.Contains(out, "[secret:FAKE_TOKEN]") {
		t.Fatalf("the run did not answer with the output redacted:\n%s", out)
	}
	if strings.Contains(out, fakeVaultValue) {
		t.Fatalf("rpc wrote the stored value:\n%s", out)
	}
}

// abhed resolve prints the agent's messages with a stored value redacted.
func TestResolvePrintsNoStoredSecret(t *testing.T) {
	fakeVault(t)
	repo, _ := resolveRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "creds.txt"), []byte("token="+fakeVaultValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "creds.txt")
	gitIn(t, repo, "commit", "-q", "-m", "creds")
	if err := os.MkdirAll(filepath.Join(repo, ".abhed"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".abhed", "config.json"), []byte(leakingModel(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	stubForge(t, &fakeForge{})
	_, msg := resolveStderr(t, func() int {
		return resolveCmd(repo, []string{"--mode", "default", "https://git.example/t/r/issues/5"})
	})
	if !strings.Contains(msg, "[secret:FAKE_TOKEN]") {
		t.Fatalf("the agent's message was not printed redacted:\n%s", msg)
	}
	if strings.Contains(msg, fakeVaultValue) {
		t.Fatalf("resolve printed the stored value:\n%s", msg)
	}
}
