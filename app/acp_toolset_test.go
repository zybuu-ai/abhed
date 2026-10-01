package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// A real ACP session plans with the todo tool and delegates with task: the
// plan reaches the editor's plan panel, and the subagent's write is shown as
// a tool call and then asked about under the same id.
func TestACPSessionPlansAndDelegates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		tools, child := 0, false
		for _, m := range req.Messages {
			if m.Role == "tool" {
				tools++
			}
			if m.Role == "user" && m.Content == "look around" {
				child = true
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		call := func(name string, args any) {
			raw, _ := json.Marshal(args)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c`+strconv.FormatInt(n.Add(1), 10)+
				`","type":"function","function":{"name":"`+name+`","arguments":`+strconv.Quote(string(raw))+`}}]}}]}`)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		}
		switch {
		case child && tools == 0:
			call("write", map[string]string{"path": filepath.Join(ws, "found.txt"), "content": "x\n"})
		case !child && tools == 0:
			call("todo", map[string]any{"items": []map[string]string{{"id": "1", "text": "look around", "status": "in_progress"}}})
		case !child && tools == 1:
			call("task", map[string]string{"prompt": "look around", "description": "look around"})
		default:
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
	}))
	defer srv.Close()
	cfg := `{"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + srv.URL + `","model":"m","context_window":8192}}}}`
	t.Setenv(config.TrustEnv, "1")
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
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
	_, updates := cl.prompt(3, map[string]any{"sessionId": sess.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "plan and delegate"}}})

	planned := false
	for _, u := range updates {
		if u["sessionUpdate"] == "plan" {
			entries, _ := u["entries"].([]any)
			if len(entries) == 1 && entries[0].(map[string]any)["content"] == "look around" {
				planned = true
			}
		}
	}
	if !planned {
		t.Fatalf("no plan update reached the editor: %v", updates)
	}
	var asked []string
	for _, line := range cl.arrived() {
		if strings.Contains(line, "subagent-") {
			asked = append(asked, line)
		}
	}
	if len(asked) != 2 || !slices.Equal([]string{strings.Fields(asked[0])[0], strings.Fields(asked[1])[0]}, []string{"call", "ask"}) ||
		strings.Fields(asked[0])[1] != strings.Fields(asked[1])[1] {
		t.Fatalf("the subagent's ask did not reach the editor as a call and then a request: %v", cl.arrived())
	}
	if _, err := os.Stat(filepath.Join(ws, "found.txt")); err != nil {
		t.Fatalf("the approved subagent write did not run: %v", err)
	}
}
