package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	abhed "github.com/zybuu-ai/abhed/sdk"
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

// brokenVault puts an unloadable store where the default one lives: corrupt,
// or readable by others.
func brokenVault(t *testing.T, kind string) string {
	t.Helper()
	managedConfig(t, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	dir := filepath.Join(os.Getenv("HOME"), ".abhed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "secrets.json")
	data, mode := `{"FAKE_TOKEN": `, os.FileMode(0o600)
	if kind == "wrong mode" {
		data, mode = `{"FAKE_TOKEN":"`+fakeVaultValue+`"}`, 0o644
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// wantRefusal checks out names the store and the fix, and never the value.
func wantRefusal(t *testing.T, entry, out, path string) {
	t.Helper()
	if !strings.Contains(out, path) || !strings.Contains(out, "chmod 600") || strings.Contains(out, fakeVaultValue) {
		t.Fatalf("%s did not refuse naming %s and the fix:\n%s", entry, path, out)
	}
}

// Every entry point refuses to start over a secrets store it cannot load, and
// the doctor reports it as not ready.
func TestEntryPointsRefuseAnUnloadableSecretsStore(t *testing.T) {
	for _, kind := range []string{"corrupt", "wrong mode"} {
		t.Run(kind, func(t *testing.T) {
			path := brokenVault(t, kind)
			ws := credsWorkspace(t, leakingModel(t))

			cl := newACPClient(t, nil)
			cl.request(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
			created := cl.request(2, "session/new", map[string]any{"cwd": ws, "mcpServers": []any{}})
			if created.Error == nil {
				t.Fatalf("acp started a session: %s", created.Result)
			}
			wantRefusal(t, "acp", created.Error.Message, path)

			out := runRPC(t, ws, `{"id":"1","method":"start"}`, `{"id":"2","method":"quit"}`)
			if strings.Contains(out, `"type":"ready"`) {
				t.Fatalf("rpc started a session:\n%s", out)
			}
			wantRefusal(t, "rpc", out, path)

			repo, _ := resolveRepo(t)
			if err := os.MkdirAll(filepath.Join(repo, ".abhed"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, ".abhed", "config.json"), []byte(leakingModel(t)), 0o600); err != nil {
				t.Fatal(err)
			}
			stubForge(t, &fakeForge{})
			code, msg := resolveStderr(t, func() int { return resolveCmd(repo, []string{"https://git.example/t/r/issues/5"}) })
			if code == 0 {
				t.Fatal("resolve ran")
			}
			wantRefusal(t, "resolve", msg, path)

			code, msg = resolveStderr(t, func() int { return evalCmd(ws, t.TempDir(), "") })
			if code == 0 {
				t.Fatal("eval ran")
			}
			wantRefusal(t, "eval", msg, path)

			code, msg = resolveStderr(t, func() int { return newApp().serveCmd(ws, "127.0.0.1:0") })
			if code == 0 {
				t.Fatal("serve started")
			}
			wantRefusal(t, "serve", msg, path)

			if err := vaultLoads(); err == nil {
				t.Fatal("the terminal's start-up check passed")
			} else {
				wantRefusal(t, "the terminal", err.Error(), path)
			}

			// The doctor's own endpoint, which never says the value.
			dws := credsWorkspace(t, `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"`+
				doctorEndpoint(t).URL+`","model":"m","context_window":8192}}}}`)
			out, code = stdoutOf(t, func() int { return newApp().doctor(dws) })
			if code == 0 || !strings.Contains(out, "secrets     UNAVAILABLE") || !strings.Contains(out, "Not ready") {
				t.Fatalf("the doctor did not report the store (%d):\n%s", code, out)
			}
			wantRefusal(t, "doctor", out, path)
		})
	}
}

// resolve prints the run's last message even when delivery lags behind it.
func TestResolvePrintsTheLastMessageOfASlowDelivery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	repo, _ := resolveRepo(t)
	srv := scriptedEndpoint(t, func(dir string) []string {
		return []string{
			toolCall("read", map[string]string{"path": filepath.Join(dir, "a.txt")}),
			`{"choices":[{"delta":{"content":"closing-message-marker"}}]}`,
		}
	})
	cfg := `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + srv.URL + `","model":"m","context_window":8192}}}}`
	if err := os.MkdirAll(filepath.Join(repo, ".abhed"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	stubForge(t, &fakeForge{})
	old := resolveEvent
	t.Cleanup(func() { resolveEvent = old })
	resolveEvent = func(ev abhed.Event) { time.Sleep(40 * time.Millisecond); old(ev) }
	_, msg := resolveStderr(t, func() int { return resolveCmd(repo, []string{"https://git.example/t/r/issues/5"}) })
	if !strings.Contains(msg, "closing-message-marker") {
		t.Fatalf("the run's last message was not printed:\n%s", msg)
	}
}

// TestVaultRefusalHelper is the terminal run the test below starts as its own
// process, since a refused start exits.
func TestVaultRefusalHelper(t *testing.T) {
	ws := os.Getenv("ABHED_VAULT_HELPER_WS")
	if ws == "" {
		t.Skip("run by TestTerminalRefusesAnUnloadableSecretsStore")
	}
	// TestMain gave this process a home of its own; the store under test is in the parent's.
	t.Setenv("HOME", os.Getenv("ABHED_VAULT_HELPER_HOME"))
	os.Exit(Main([]string{"-C", ws, "-p", "read creds.txt"}))
}

// abhed -p, run as a process, refuses to start over a store it cannot load and
// never reaches the model.
func TestTerminalRefusesAnUnloadableSecretsStore(t *testing.T) {
	for _, kind := range []string{"corrupt", "wrong mode"} {
		t.Run(kind, func(t *testing.T) {
			path := brokenVault(t, kind)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				http.Error(w, "unused", http.StatusInternalServerError)
			}))
			t.Cleanup(srv.Close)
			ws := credsWorkspace(t, `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"`+
				srv.URL+`","model":"m","context_window":8192}}}}`)
			cmd := exec.Command(os.Args[0], "-test.run=^TestVaultRefusalHelper$")
			cmd.Env = append(os.Environ(), "ABHED_VAULT_HELPER_WS="+ws, "ABHED_VAULT_HELPER_HOME="+os.Getenv("HOME"))
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("abhed -p did not exit 1 (%v):\n%s", err, out)
			}
			wantRefusal(t, "abhed -p", string(out), path)
			if calls.Load() != 0 {
				t.Fatal("the refused run reached the model")
			}
		})
	}
}

// abhed secret set refuses a value too short to redact without matching
// ordinary text, and stores one long enough.
func TestSecretSetRefusesAShortValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	t.Setenv("ABHED_SECRETS_FILE", path)
	set := func(value string) (int, string) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.WriteString(value + "\n")
		_ = w.Close()
		old := os.Stdin
		os.Stdin = r
		defer func() { os.Stdin = old }()
		return resolveStderr(t, func() int { return secretCmd([]string{"set", "FAKE_TOKEN"}) })
	}
	if code, msg := set("short"); code == 0 || !strings.Contains(msg, "at least 8") {
		t.Fatalf("a 5-character value was stored (%d): %s", code, msg)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the refused value reached the store")
	}
	if code, msg := set("long-enough"); code != 0 {
		t.Fatalf("an 11-character value was refused (%d): %s", code, msg)
	}
}

// abhed secret set and rm on a store that cannot be loaded name the same fix
// as a refused start.
func TestSecretCommandsNameTheFixForABrokenStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(`{"FAKE_TOKEN": `), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ABHED_SECRETS_FILE", path)
	for _, args := range [][]string{{"rm", "FAKE_TOKEN"}, {"list"}} {
		code, msg := resolveStderr(t, func() int { return secretCmd(args) })
		if code == 0 || !strings.Contains(msg, path) || !strings.Contains(msg, "remove it and add the secrets again") {
			t.Fatalf("secret %v (%d) did not name the file and the fix: %s", args, code, msg)
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString("long-enough-value\n")
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	code, msg := resolveStderr(t, func() int { return secretCmd([]string{"set", "FAKE_TOKEN"}) })
	if code == 0 || !strings.Contains(msg, "remove it and add the secrets again") {
		t.Fatalf("secret set (%d) did not name the fix: %s", code, msg)
	}
}
