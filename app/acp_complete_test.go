package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// fimServer is a completion model's fill-in-the-middle endpoint: it answers
// every request with text, after delay, and keeps what it was sent.
type fimServer struct {
	*httptest.Server
	mu    sync.Mutex
	seen  []map[string]any
	delay time.Duration
}

func newFIMServer(t *testing.T, text string) *fimServer {
	t.Helper()
	f := &fimServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions" {
			http.Error(w, "only completions", http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		f.mu.Lock()
		f.seen = append(f.seen, m)
		d := f.delay
		f.mu.Unlock()
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"text": text}}, "usage": map[string]any{"prompt_tokens": 30, "completion_tokens": 4}})
		_, _ = w.Write(out)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fimServer) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.seen...)
}

// completionRig is a Studio rig whose own configuration names a completion
// model at fim, with more of the configuration in extra.
func completionRig(t *testing.T, fim, extra string) *studioRig {
	t.Helper()
	m := newScriptModel(t)
	userCfg := `{"model":{"default":"m","completion":"coder","providers":{"m":` + stubProvider(m.URL, "m-1") +
		`,"coder":{"type":"openai-compatible","base_url":"` + fim + `/v1","model":"qwen2.5-coder"}}}` + extra + `}`
	acpModelsEnv(t, userCfg)
	ws := t.TempDir()
	if real, err := filepath.EvalSymlinks(ws); err == nil {
		ws = real
	}
	r := &studioRig{t: t, ws: ws, home: os.Getenv("HOME"), model: m}
	r.cl = newStudioClient(t, ws, true)
	r.cl.ok("initialize", map[string]any{"protocolVersion": 1}, nil)
	return r
}

type completeItems struct {
	Items    []map[string]any `json:"items"`
	Disabled string           `json:"disabled"`
	Refused  string           `json:"refused"`
	Limited  string           `json:"limited"`
}

// §7.10: the code around the cursor, bounded, goes to the completion model's
// fill-in-the-middle endpoint and its text comes back; the first use names
// the provider in the record, and the session's summary counts the rest.
func TestStudioCompletes(t *testing.T) {
	fim := newFIMServer(t, "return a + b")
	r := completionRig(t, fim.URL, "")
	id := r.open()
	uri := "file://" + r.ws + "/add.go"
	var res completeItems
	long := strings.Repeat("// filler\n", 2000)
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": uri, "language": "go", "trigger": "auto",
		"position": map[string]int{"line": 3, "character": 1}, "prefix": long + "func add(a, b int) int {\n\t", "suffix": "\n}\n",
		"context": []map[string]string{{"uri": "file://" + r.ws + "/util.go", "text": "package main\n\nfunc helper() {}\n"}, {"uri": "file:///etc/passwd", "text": "root:x:0:0"}}}, &res)
	if len(res.Items) != 1 || res.Items[0]["text"] != "return a + b" {
		t.Fatalf("items %+v", res)
	}
	seen := fim.requests()
	if len(seen) != 1 {
		t.Fatalf("%d requests", len(seen))
	}
	prompt, _ := seen[0]["prompt"].(string)
	if len(prompt) > completePrefix+200 || !strings.HasSuffix(prompt, "func add(a, b int) int {\n\t") || !strings.Contains(prompt, "// util.go") || strings.Contains(prompt, "root:x") {
		t.Fatalf("prompt %d bytes: %.200q", len(prompt), prompt)
	}
	if seen[0]["suffix"] != "\n}\n" {
		t.Fatalf("suffix %v", seen[0]["suffix"])
	}
	// A file a rule keeps, or one of keys, is never sent.
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/.env", "prefix": "TOKEN=", "suffix": ""}, &res)
	if res.Refused == "" || len(fim.requests()) != 1 {
		t.Fatalf("a key file was sent: %+v", res)
	}
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file:///etc/hosts", "prefix": "x", "suffix": ""}, &res)
	if res.Refused == "" {
		t.Fatalf("a file outside the workspace was sent: %+v", res)
	}
	r.cl.call("session/close", map[string]any{"sessionId": id})
	enabled, _ := r.recorded(id, agent.EvCompletionEnabled)
	if len(enabled) != 1 || enabled[0]["provider"] != "coder" || enabled[0]["mode"] != "fim" || enabled[0]["local"] != true {
		t.Fatalf("completion.enabled %v", enabled)
	}
	sum, _ := r.recorded(id, agent.EvCompletionSummary)
	if len(sum) != 1 || sum[0]["requests"] != float64(1) || sum[0]["served"] != float64(1) || sum[0]["refused"] != float64(2) || sum[0]["tokens_in"] != float64(30) {
		t.Fatalf("completion.summary %v", sum)
	}
}

// Off unless configured; a managed or user local_only keeps a remote model out.
func TestStudioCompletionsOff(t *testing.T) {
	fim := newFIMServer(t, "x")
	r := completionRig(t, fim.URL, `,"completion":{"enabled":false}`)
	id := r.open()
	var res completeItems
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "x", "suffix": ""}, &res)
	if !strings.Contains(res.Disabled, "turned off") || len(fim.requests()) != 0 {
		t.Fatalf("%+v", res)
	}

	remote := completionRig(t, strings.Replace(fim.URL, "127.0.0.1", "abhed-remote.invalid", 1), `,"completion":{"local_only":true}`)
	rid := remote.open()
	remote.cl.ok("_abhed/complete", map[string]any{"sessionId": rid, "uri": "file://" + remote.ws + "/a.go", "prefix": "x", "suffix": ""}, &res)
	if !strings.Contains(res.Disabled, "local_only") {
		t.Fatalf("%+v", res)
	}
}

// A newer request replaces the one in flight, and the rate limit holds.
func TestStudioCompletionsSupersedeAndLimit(t *testing.T) {
	fim := newFIMServer(t, "y")
	fim.delay = 500 * time.Millisecond
	r := completionRig(t, fim.URL, "")
	id := r.open()
	params := func(p string) map[string]any {
		return map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": p, "suffix": ""}
	}
	from := r.cl.mark()
	first, _ := json.Marshal(params("a"))
	r.cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(`"c1"`), Method: "_abhed/complete", Params: first})
	time.Sleep(150 * time.Millisecond)
	var res completeItems
	r.cl.ok("_abhed/complete", params("ab"), &res)
	if len(res.Items) != 1 {
		t.Fatalf("the newer request: %+v", res)
	}
	old := r.cl.waitFor(from, "the first answer", func(m rpcMessage) bool { return string(m.ID) == `"c1"` })
	if !strings.Contains(string(old.Result), "cancelled") {
		t.Fatalf("the replaced request: %s", old.Result)
	}
	fim.delay = 0
	limited := false
	for range 10 {
		r.cl.ok("_abhed/complete", params("abc"), &res)
		limited = limited || res.Limited != ""
	}
	if !limited {
		t.Fatal("ten requests at once were not limited")
	}
}
