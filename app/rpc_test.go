package app

import (
	"bufio"
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

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// scriptedModel answers the first request with one bash call and every later
// one with a closing message, as an OpenAI-compatible server would.
func scriptedModel(t *testing.T, command string) *httptest.Server {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"command": command, "description": "probe"})
	call, _ := json.Marshal(string(args))
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
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
	return srv
}

// runRPC feeds lines to abhed rpc and returns everything it wrote.
func runRPC(t *testing.T, workspace string, lines ...string) string {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()

	var got strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for sc.Scan() {
			got.WriteString(sc.Text() + "\n")
		}
	}()
	go func() {
		for _, l := range lines {
			fmt.Fprintln(inW, l)
		}
		_ = inW.Close()
	}()
	rpcCmd(workspace)
	_ = outW.Close()
	<-done
	return got.String()
}

// abhed rpc runs bash in the configured sandbox, as the terminal does: it
// once ran it on the host with the operator's whole environment.
func TestRPCRunsBashInTheConfiguredSandbox(t *testing.T) {
	managedConfig(t, "")
	outside := t.TempDir()
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	// Keep container runtimes off the path so the process tier is the one chosen.
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
	if sb, err := sandbox.Select(sandbox.DefaultPolicy(ws)); err != nil || sb.Tier() != sandbox.TierProcess {
		t.Skipf("the process tier is not what this host selects: %v", err)
	}
	// The per-user temp directory is writable in the sandbox; point it at the
	// workspace so the outside directory is really outside.
	t.Setenv("TMPDIR", ws)
	t.Setenv("RPC_TEST_MODEL_KEY", "sk-must-not-leak")

	target := filepath.Join(outside, "escaped")
	srv := scriptedModel(t, `echo "tier=[$ABHED_SANDBOX] key=[$RPC_TEST_MODEL_KEY]"; echo x > `+target)
	cfg := `{"model": {"default": "fake", "providers": {"fake": {"type": "openai-compatible",
		"base_url": "` + srv.URL + `", "model": "m", "api_key_env": "RPC_TEST_MODEL_KEY"}}},
		"sandbox": {"min_tier": "process"}}`
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runRPC(t, ws,
		`{"id":"1","method":"start","allow":["bash"]}`,
		`{"id":"2","method":"prompt","prompt":"probe"}`,
		`{"id":"3","method":"quit"}`)
	if !strings.Contains(out, `"type":"ready"`) {
		t.Fatalf("rpc did not start:\n%s", out)
	}
	if !strings.Contains(out, "tier=[process]") {
		t.Errorf("bash did not run in the process sandbox:\n%s", out)
	}
	if strings.Contains(out, "sk-must-not-leak") {
		t.Errorf("bash saw the model key:\n%s", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Errorf("bash wrote outside the workspace: %s", target)
	}
}

// A tier the host cannot give is refused at start, not quietly dropped.
func TestRPCRefusesATierItCannotHonour(t *testing.T) {
	managedConfig(t, "")
	ws := t.TempDir()
	if _, err := sandbox.Select(sandbox.Policy{MinTier: sandbox.TierVM, Workspace: ws}); err == nil {
		t.Skip("this host can give the vm tier")
	}
	cfg := `{"model": {"default": "fake", "providers": {"fake": {"type": "openai-compatible",
		"base_url": "http://127.0.0.1:1", "model": "m"}}}, "sandbox": {"min_tier": "vm"}}`
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runRPC(t, ws, `{"id":"1","method":"start"}`, `{"id":"2","method":"quit"}`)
	if strings.Contains(out, `"type":"ready"`) || !strings.Contains(out, `"type":"error"`) {
		t.Fatalf("rpc started without the configured sandbox:\n%s", out)
	}
}
