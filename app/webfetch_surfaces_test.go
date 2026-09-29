package app

import (
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
	"github.com/zybuu-ai/abhed/internal/webfetch"
)

// webFetchWorkspace is a trusted workspace whose model calls web_fetch once
// with no host list configured.
func webFetchWorkspace(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"url": "https://docs.example.invalid/"})
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if n.Add(1) == 1 {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"f1","type":"function","function":{"name":"web_fetch","arguments":`+
				strconv.Quote(string(args))+`}}]}}]}`)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	cfg := `{"web_fetch":{"enabled":true},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` +
		srv.URL + `","model":"m","context_window":8192}}}}`
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.TrustEnv, "1") // the test wrote this configuration
	return ws
}

// On rpc, which has nobody to ask, web_fetch with no host list is an ask and
// so is refused, not run.
func TestRPCWebFetchAsksWithoutAHostList(t *testing.T) {
	ws := webFetchWorkspace(t)
	out := runRPC(t, ws,
		`{"id":"1","method":"start"}`,
		`{"id":"2","method":"prompt","prompt":"read it"}`,
		`{"id":"3","method":"quit"}`)
	reason, _ := json.Marshal(webfetch.AskReason)
	if !strings.Contains(out, `"requires_approval":true`) || !strings.Contains(out, strings.Trim(string(reason), `"`)) {
		t.Fatalf("rpc ran web_fetch without asking:\n%s", out)
	}
	if !strings.Contains(out, `"type":"action.denied"`) {
		t.Fatalf("rpc did not refuse the ask:\n%s", out)
	}
}

// In an ACP editor, web_fetch with no host list is a permission request.
func TestACPWebFetchAsksWithoutAHostList(t *testing.T) {
	ws := webFetchWorkspace(t)
	var mu sync.Mutex
	var asked []string
	cl := newACPClient(t, func(method string, params json.RawMessage) any {
		if method == "session/request_permission" {
			var p struct {
				ToolCall struct {
					Meta map[string]struct {
						Tool   string `json:"tool"`
						Reason string `json:"reason"`
					} `json:"_meta"`
				} `json:"toolCall"`
			}
			_ = json.Unmarshal(params, &p)
			m := p.ToolCall.Meta["zybuu.ai/abhed"]
			mu.Lock()
			asked = append(asked, m.Tool+": "+m.Reason)
			mu.Unlock()
		}
		return chosen(params, "reject_once")
	})
	cl.request(1, "initialize", map[string]any{"protocolVersion": 1})
	created := cl.request(2, "session/new", map[string]any{"cwd": ws})
	if created.Error != nil {
		t.Fatalf("session/new failed: %s", created.Error.Message)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(created.Result, &sess)
	cl.prompt(3, map[string]any{"sessionId": sess.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "read it"}}})
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || !strings.HasPrefix(asked[0], "web_fetch: web_fetch asks: no allowed_hosts") {
		t.Fatalf("the editor was not asked before web_fetch: %q", asked)
	}
}
