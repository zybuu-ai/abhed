package app

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// stderrDuring runs fn with os.Stderr captured.
func stderrDuring(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(done) }()
	fn()
	os.Stderr = old
	_ = w.Close()
	<-done
	return buf.String()
}

// insecureClusterWorkspace is a trusted workspace declaring a login cluster
// that skips TLS verification.
func insecureClusterWorkspace(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := `{"k8s":{"enabled":true,"kubeconfig":"` + filepath.Join(ws, "no-kubeconfig") + `",` +
		`"clusters":[{"name":"lab","server":"https://lab.example:6443","insecure_skip_tls_verify":true}]},` +
		`"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"http://127.0.0.1:1","model":"m","context_window":8192}}}}`
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.TrustEnv, "1") // the test wrote this configuration
	return ws
}

const insecureClusterWarning = `k8s cluster "lab" skips TLS verification`

// rpc says on stderr, never on its protocol stream, that a login cluster
// skips TLS verification, as the terminal and serve do.
func TestRPCWarnsOfAnInsecureClusterOnStderr(t *testing.T) {
	ws := insecureClusterWorkspace(t)
	var out string
	errOut := stderrDuring(t, func() {
		out = runRPC(t, ws, `{"id":"1","method":"start"}`, `{"id":"2","method":"quit"}`)
	})
	if !strings.Contains(out, `"type":"ready"`) {
		t.Fatalf("rpc did not start:\n%s", out)
	}
	if !strings.Contains(errOut, insecureClusterWarning) {
		t.Fatalf("no warning on stderr: %q", errOut)
	}
	if strings.Contains(out, insecureClusterWarning) {
		t.Fatalf("the warning reached the protocol stream:\n%s", out)
	}
}

// acp says it on stderr when a session starts.
func TestACPWarnsOfAnInsecureClusterOnStderr(t *testing.T) {
	ws := insecureClusterWorkspace(t)
	errOut := stderrDuring(t, func() {
		cl := newACPClient(t, nil)
		cl.request(1, "initialize", map[string]any{"protocolVersion": 1})
		created := cl.request(2, "session/new", map[string]any{"cwd": ws})
		if created.Error != nil {
			t.Errorf("session/new failed: %s", created.Error.Message)
		}
		raw, _ := json.Marshal(created)
		if strings.Contains(string(raw), insecureClusterWarning) {
			t.Errorf("the warning reached the protocol stream: %s", raw)
		}
	})
	if !strings.Contains(errOut, insecureClusterWarning) {
		t.Fatalf("no warning on stderr: %q", errOut)
	}
}
