package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// sshHintRun runs -p against a model that makes one call, with app1 in
// ssh.hosts, and returns stderr, the result line and what the model was
// sent after the call.
func sshHintRun(t *testing.T, tool, args string) (string, resultLine, string) {
	t.Helper()
	var n atomic.Int64
	var mu sync.Mutex
	var after string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if n.Add(1) == 1 {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"`+
				tool+`","arguments":`+strconv.Quote(args)+`}}]}}]}`)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		mu.Lock()
		after = string(body)
		mu.Unlock()
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	home, ws := t.TempDir(), t.TempDir()
	// Port 9 on loopback: nothing is ever reached, and nothing should be.
	cfg := `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` +
		srv.URL + `","model":"m","context_window":8192}}},` +
		`"ssh":{"enabled":true,"hosts":[{"name":"app1","addr":"127.0.0.1:9","user":"root"}]}}`
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := mainHelper([]string{"-C", ws, "-p", "check app1", "-output-format", "json"},
		"HOME="+home, "XDG_CONFIG_HOME="+t.TempDir(), "ABHED_SECRETS_FILE=", config.TrustEnv+"=", "SSH_AUTH_SOCK=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run()
	var res resultLine
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &res); err != nil {
		t.Fatalf("no result line: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	return stderr.String(), res, after
}

// ssh_connect naming a declared host is answered "it is ready; use ssh",
// before anyone is asked, and no flag is offered: an allow for ssh_connect
// could never make it connect a declared name.
func TestHeadlessSSHConnectDeclaredHost(t *testing.T) {
	stderr, res, after := sshHintRun(t, "ssh_connect", `{"addr":"app1"}`)
	if strings.Contains(stderr, "-allow") || strings.Contains(stderr, "-mode") {
		t.Fatalf("a flag that cannot help was offered:\n%s", stderr)
	}
	if !strings.Contains(after, "app1 is a host the operator declared, and it is ready") ||
		!strings.Contains(after, "ssh tool") {
		t.Fatalf("the model was not told to use ssh:\n%s", after)
	}
	for _, d := range res.PermissionDenials {
		if d.Flag != "" || !strings.Contains(d.Hint, "no flag") {
			t.Fatalf("denial offers a flag: %+v", d)
		}
	}
	// The ssh tool's description lists the host as ready.
	if !strings.Contains(after, "ready now with no ssh_connect needed: app1") {
		t.Errorf("the ssh tool description does not list app1 as ready")
	}
}

// A log read refused for want of ssh.read_contents says so, and the flag it
// offers is one that would allow exactly that call.
func TestHeadlessSSHContentReadNamesTheRule(t *testing.T) {
	stderr, res, _ := sshHintRun(t, "ssh", `{"host":"app1","command":"tail -n 100 /var/log/app/app.log"}`)
	if len(res.PermissionDenials) != 1 {
		t.Fatalf("permission_denials: %+v\n%s", res.PermissionDenials, stderr)
	}
	d := res.PermissionDenials[0]
	if !strings.Contains(d.Reason, "ssh.read_contents: true") || strings.Contains(d.Reason, "can make changes") {
		t.Errorf("reason does not name ssh.read_contents: %q", d.Reason)
	}
	if d.Flag != "-allow 'ssh(app1/tail -n 100 /var/log/app/app.log)'" {
		t.Errorf("flag %q", d.Flag)
	}
}
