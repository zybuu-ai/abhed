package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/secretfiles"
)

// fimServer is a completion model's fill-in-the-middle endpoint: it answers
// every request with text, after delay, and keeps what it was sent.
type fimServer struct {
	*httptest.Server
	mu      sync.Mutex
	seen    []map[string]any
	auth    []string
	paths   []string
	gate    chan struct{} // when set, a request waits for it to close
	status  int           // when set, /v1/completions answers with it
	body    string        // and with this body
	arrived chan struct{}
}

func newFIMServer(t *testing.T, text string) *fimServer {
	t.Helper()
	f := &fimServer{arrived: make(chan struct{}, 64)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		gate, status, body := f.gate, f.status, f.body
		if r.URL.Path == "/v1/completions" && status == 0 {
			f.seen = append(f.seen, m)
		}
		f.mu.Unlock()
		f.arrived <- struct{}{}
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		switch {
		case r.URL.Path == "/v1/completions" && status != 0:
			http.Error(w, body, status)
		case r.URL.Path == "/v1/completions":
			out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"text": text}}, "usage": map[string]any{"prompt_tokens": 30, "completion_tokens": 4}})
			_, _ = w.Write(out)
		case r.URL.Path == "/v1/chat/completions":
			out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}, "finish_reason": "stop"}}})
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", out)
		default:
			http.Error(w, "not here", http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// noHold skips the debounce, so a test's requests go at once.
func noHold(t *testing.T) {
	old := completeHold
	completeHold = func(context.Context) error { return nil }
	t.Cleanup(func() { completeHold = old })
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
	Cancel   string           `json:"cancelled"`
}

// §7.10: the code around the cursor, bounded, goes to the completion model's
// fill-in-the-middle endpoint and its text comes back; the first use names
// the provider in the record, and the session's summary counts the rest.
func TestStudioCompletes(t *testing.T) {
	fim := newFIMServer(t, "return a + b")
	r := completionRig(t, fim.URL, "")
	noHold(t)
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
	noHold(t)
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

// A newer request replaces the one in flight: the first is cancelled once it
// has reached the model, and only the newer one's text comes back.
func TestStudioCompletionsSupersede(t *testing.T) {
	fim := newFIMServer(t, "y")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	id := r.open()
	params := func(p string) map[string]any {
		return map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": p, "suffix": ""}
	}
	fim.mu.Lock()
	fim.gate = make(chan struct{})
	fim.mu.Unlock()
	from := r.cl.mark()
	first, _ := json.Marshal(params("a"))
	r.cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(`"c1"`), Method: "_abhed/complete", Params: first})
	<-fim.arrived
	fim.mu.Lock()
	fim.gate = nil
	fim.mu.Unlock()
	var res completeItems
	r.cl.ok("_abhed/complete", params("ab"), &res)
	if len(res.Items) != 1 {
		t.Fatalf("the newer request: %+v", res)
	}
	old := r.cl.waitFor(from, "the first answer", func(m rpcMessage) bool { return string(m.ID) == `"c1"` })
	if !strings.Contains(string(old.Result), "cancelled") {
		t.Fatalf("the replaced request: %s", old.Result)
	}
	r.cl.call("session/close", map[string]any{"sessionId": id})
	sum, _ := r.recorded(id, agent.EvCompletionSummary)
	if len(sum) != 1 || sum[0]["requests"] != float64(2) || sum[0]["cancelled"] != float64(1) || sum[0]["superseded"] != float64(0) {
		t.Fatalf("completion.summary %v", sum)
	}
}

// A request replaced before it went is superseded, not sent, and does not
// record completion.enabled; the one that goes records it first.
func TestStudioCompletionsSupersededBeforeSending(t *testing.T) {
	fim := newFIMServer(t, "z")
	r := completionRig(t, fim.URL, "")
	held := make(chan struct{})
	var once sync.Once
	old := completeHold
	completeHold = func(ctx context.Context) error {
		first := false
		once.Do(func() { first = true })
		if !first {
			return nil
		}
		close(held)
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() { completeHold = old })
	id := r.open()
	params := func(p string) map[string]any {
		return map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": p, "suffix": ""}
	}
	from := r.cl.mark()
	first, _ := json.Marshal(params("a"))
	r.cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(`"c1"`), Method: "_abhed/complete", Params: first})
	<-held
	enabled, _ := r.recorded(id, agent.EvCompletionEnabled)
	if len(enabled) != 0 {
		t.Fatalf("completion.enabled was recorded before anything was sent: %v", enabled)
	}
	var res completeItems
	r.cl.ok("_abhed/complete", params("ab"), &res)
	if len(res.Items) != 1 {
		t.Fatalf("the newer request: %+v", res)
	}
	r.cl.waitFor(from, "the first answer", func(m rpcMessage) bool { return string(m.ID) == `"c1"` })
	if n := len(fim.requests()); n != 1 {
		t.Fatalf("%d requests reached the model; want 1", n)
	}
	r.cl.call("session/close", map[string]any{"sessionId": id})
	enabled, _ = r.recorded(id, agent.EvCompletionEnabled)
	sum, _ := r.recorded(id, agent.EvCompletionSummary)
	if len(enabled) != 1 || len(sum) != 1 || sum[0]["superseded"] != float64(1) || sum[0]["requests"] != float64(1) {
		t.Fatalf("completion.enabled %v, summary %v", enabled, sum)
	}
}

// Studio's seq orders requests: an older one arriving after a newer one is dropped.
func TestStudioCompletionsSeq(t *testing.T) {
	fim := newFIMServer(t, "s")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	id := r.open()
	var res completeItems
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "ab", "seq": 7}, &res)
	if len(res.Items) != 1 {
		t.Fatalf("seq 7: %+v", res)
	}
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "a", "seq": 6}, &res)
	if res.Cancel == "" || len(fim.requests()) != 1 {
		t.Fatalf("seq 6 after 7 was sent: %+v", res)
	}
}

// The token bucket refills with the clock it is given, never past its burst.
func TestCompletionBucket(t *testing.T) {
	l := rateLimit{burst: 3, every: time.Second}
	var b bucket
	at := time.Unix(1000, 0)
	for i := range 3 {
		if !b.take(l, at) {
			t.Fatalf("take %d of the burst was refused", i)
		}
	}
	if b.take(l, at) {
		t.Fatal("a fourth take at once was allowed")
	}
	if b.take(l, at.Add(900*time.Millisecond)) {
		t.Fatal("a take before a token refilled was allowed")
	}
	if !b.take(l, at.Add(1100*time.Millisecond)) {
		t.Fatal("a refilled token was refused")
	}
	later, took := at.Add(time.Hour), 0
	for range 5 {
		if b.take(l, later) {
			took++
		}
	}
	if took != 3 {
		t.Fatalf("an hour refilled %d tokens; want the burst of 3", took)
	}
}

// With the clock stopped, a session gets its burst and then is limited; the
// connection's own limit holds across its sessions.
func TestStudioCompletionsLimit(t *testing.T) {
	fim := newFIMServer(t, "y")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	at := time.Unix(5000, 0)
	old := completeNow
	completeNow = func() time.Time { return at }
	t.Cleanup(func() { completeNow = old })
	ids := []string{r.open(), r.open(), r.open()}
	served := 0
	for _, id := range ids {
		limited := 0
		for range int(sessionRate.burst) + 2 {
			var res completeItems
			r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "x"}, &res)
			if res.Limited != "" {
				limited++
			} else {
				served++
			}
		}
		if limited == 0 {
			t.Fatalf("session %s was not limited", id)
		}
	}
	if served != int(connectionRate.burst) {
		t.Fatalf("%d served over three sessions; want the connection's burst of %v", served, connectionRate.burst)
	}
}

// The provider's own api_key_env is what goes; an ambient OPENAI_API_KEY never does.
func TestStudioCompletionsUseTheNamedKey(t *testing.T) {
	fim := newFIMServer(t, "k")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	cfg := filepath.Join(os.Getenv("HOME"), ".abhed", "config.json")
	b, _ := os.ReadFile(cfg)
	b = []byte(strings.Replace(string(b), `"model":"qwen2.5-coder"`, `"model":"qwen2.5-coder","api_key_env":"CODER_KEY"`, 1))
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODER_KEY", "coder-key-value")
	t.Setenv("OPENAI_API_KEY", "ambient-key-value")
	id := r.open()
	var res completeItems
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "x"}, &res)
	fim.mu.Lock()
	auth := append([]string(nil), fim.auth...)
	fim.mu.Unlock()
	if len(res.Items) != 1 || len(auth) != 1 || auth[0] != "Bearer coder-key-value" {
		t.Fatalf("items %+v, authorization sent %d times, named key used: %v", res, len(auth), len(auth) == 1 && auth[0] == "Bearer coder-key-value")
	}

	// Without a named key, no ambient one is borrowed.
	bare := newFIMServer(t, "k")
	r2 := completionRig(t, bare.URL, "")
	noHold(t)
	t.Setenv("OPENAI_API_KEY", "ambient-key-value")
	id2 := r2.open()
	r2.cl.ok("_abhed/complete", map[string]any{"sessionId": id2, "uri": "file://" + r2.ws + "/a.go", "prefix": "x"}, &res)
	bare.mu.Lock()
	defer bare.mu.Unlock()
	for _, a := range bare.auth {
		if strings.Contains(a, "ambient-key-value") {
			t.Fatal("the ambient OPENAI_API_KEY was sent")
		}
	}
}

// A managed model.default pins completions too, unless the managed file
// names the completion model itself.
func TestStudioCompletionsUnderAManagedPin(t *testing.T) {
	fim := newFIMServer(t, "p")
	for _, tc := range []struct {
		managed string
		off     bool
	}{
		{`{"model":{"default":"m"}}`, true},
		{`{"model":{"default":"m","completion":"coder"}}`, false},
	} {
		r := completionRig(t, fim.URL, "")
		noHold(t)
		path := filepath.Join(t.TempDir(), "managed.json")
		if err := os.WriteFile(path, []byte(tc.managed), 0o644); err != nil {
			t.Fatal(err)
		}
		managed.ConfigFile = path // acpModelsEnv restores it
		id := r.open()
		var res completeItems
		r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "x"}, &res)
		if off := strings.Contains(res.Disabled, "managed"); off != tc.off {
			t.Fatalf("managed %s: %+v", tc.managed, res)
		}
	}
}

// A file reached through a link is checked in every form: a link out of the
// workspace, a link to a key file, and Abhed's own state are not sent, as
// the document or as context.
func TestStudioCompletionsFollowLinks(t *testing.T) {
	fim := newFIMServer(t, "l")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	outside := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(outside, "notes.go"), []byte("package x"), 0o600))
	must(os.WriteFile(filepath.Join(r.ws, "id_ed25519"), []byte("key"), 0o600))
	must(os.Symlink(filepath.Join(outside, "notes.go"), filepath.Join(r.ws, "out.go")))
	must(os.Symlink(filepath.Join(r.ws, "id_ed25519"), filepath.Join(r.ws, "key.go")))
	must(os.MkdirAll(filepath.Join(r.ws, ".abhed"), 0o700))
	must(os.WriteFile(filepath.Join(r.ws, ".abhed", "settings.go"), []byte("x"), 0o600))
	id := r.open()
	for _, name := range []string{"out.go", "key.go", ".abhed/settings.go"} {
		var res completeItems
		r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/" + name, "prefix": "x"}, &res)
		if res.Refused == "" {
			t.Fatalf("%s was sent as the document: %+v", name, res)
		}
	}
	var res completeItems
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "x",
		"context": []map[string]string{
			{"uri": "file://" + r.ws + "/out.go", "text": "OUTSIDE-TEXT"},
			{"uri": "file://" + r.ws + "/key.go", "text": "KEY-TEXT"},
			{"uri": "file://" + r.ws + "/.abhed/settings.go", "text": "STATE-TEXT"},
		}}, &res)
	seen := fim.requests()
	if len(res.Items) != 1 || len(seen) != 1 {
		t.Fatalf("items %+v, %d requests", res, len(seen))
	}
	prompt, _ := seen[0]["prompt"].(string)
	for _, s := range []string{"OUTSIDE-TEXT", "KEY-TEXT", "STATE-TEXT"} {
		if strings.Contains(prompt, s) {
			t.Fatalf("%s was sent as context", s)
		}
	}
}

// A stored secret's value, and anything shaped like a key, is withheld from
// what is sent, and a reply carrying one is dropped.
func TestStudioCompletionsWithholdSecrets(t *testing.T) {
	fim := newFIMServer(t, "token := \"stored-secret-value-123\"")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	store := filepath.Join(os.Getenv("HOME"), ".abhed", "secrets.json")
	if err := os.WriteFile(store, []byte(`{"FAKE_TOKEN":"stored-secret-value-123"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ABHED_SECRETS_FILE", store)
	id := r.open()
	key := "AKIA" + "ABCDEFGHIJKLMNOP"
	var res completeItems
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go",
		"prefix": "x := \"stored-secret-value-123\"\naws := \"" + key + "\"\n", "suffix": "// stored-secret-value-123",
		"context": []map[string]string{{"uri": "file://" + r.ws + "/b.go", "text": "y := \"stored-secret-value-123\""}}}, &res)
	seen := fim.requests()
	if len(seen) != 1 {
		t.Fatalf("%d requests", len(seen))
	}
	sent := fmt.Sprint(seen[0]["prompt"], seen[0]["suffix"])
	if strings.Contains(sent, "stored-secret-value-123") || strings.Contains(sent, key) {
		t.Fatalf("a secret was sent: %q", sent)
	}
	if len(res.Items) != 0 {
		t.Fatalf("a reply with a secret was offered: %+v", res)
	}
}

// Context is capped in bytes per file and in all, on character boundaries,
// and the prefix and suffix in characters.
func TestStudioCompletionsCaps(t *testing.T) {
	fim := newFIMServer(t, "c")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	id := r.open()
	wide := strings.Repeat("é", completeFileBytes) // two bytes each
	var ctx []map[string]string
	for i := range completeFiles {
		ctx = append(ctx, map[string]string{"uri": fmt.Sprintf("file://%s/f%d.go", r.ws, i), "text": wide})
	}
	var res completeItems
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go",
		"prefix": strings.Repeat("ü", completePrefix+50), "suffix": strings.Repeat("ö", completeSuffix+50), "context": ctx}, &res)
	seen := fim.requests()
	if len(seen) != 1 {
		t.Fatalf("%d requests", len(seen))
	}
	prompt, _ := seen[0]["prompt"].(string)
	suffix, _ := seen[0]["suffix"].(string)
	if !utf8.ValidString(prompt) || !utf8.ValidString(suffix) {
		t.Fatal("a cap cut through a character")
	}
	if n := utf8.RuneCountInString(suffix); n != completeSuffix {
		t.Fatalf("suffix %d characters", n)
	}
	lead := strings.TrimRight(prompt, "ü")
	if n := utf8.RuneCountInString(prompt) - utf8.RuneCountInString(lead); n != completePrefix {
		t.Fatalf("prefix %d characters", n)
	}
	if len(lead) > completeAllFiles {
		t.Fatalf("context %d bytes; cap %d", len(lead), completeAllFiles)
	}
	for _, part := range strings.Split(lead, "// f")[1:] {
		if len(part) > completeFileBytes+16 {
			t.Fatalf("one file sent %d bytes", len(part))
		}
	}
}

// Once a session has closed, its completions are refused.
func TestStudioCompletionsAfterClose(t *testing.T) {
	fim := newFIMServer(t, "c")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	id := r.open()
	r.cl.call("session/close", map[string]any{"sessionId": id})
	res := r.cl.call("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "x"})
	if res.Error == nil || len(fim.requests()) != 0 {
		t.Fatalf("a closed session completed: %+v", res)
	}
}

// A provider without fill in the middle is asked by chat, once it has said so.
func TestStudioCompletionsFallBackToChat(t *testing.T) {
	fim := newFIMServer(t, "chat text")
	fim.status, fim.body = http.StatusNotFound, "404 page not found"
	r := completionRig(t, fim.URL, "")
	noHold(t)
	id := r.open()
	for range 2 {
		var res completeItems
		r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "x"}, &res)
		if len(res.Items) != 1 || res.Items[0]["text"] != "chat text" {
			t.Fatalf("%+v", res)
		}
	}
	fim.mu.Lock()
	paths := fmt.Sprint(fim.paths)
	fim.mu.Unlock()
	if paths != "[/v1/completions /v1/chat/completions /v1/chat/completions]" {
		t.Fatalf("paths %v", paths)
	}
	// The record says chat before the first chat request went.
	r.cl.call("session/close", map[string]any{"sessionId": id})
	enabled, _ := r.recorded(id, agent.EvCompletionEnabled)
	if len(enabled) != 2 || enabled[0]["mode"] != "fim" || enabled[1]["mode"] != "chat" {
		t.Fatalf("completion.enabled %v", enabled)
	}
}

// A 400 for another reason, such as a prompt too long, is a failure, not a
// sign the endpoint has no fill in the middle.
func TestStudioCompletionsBadRequestIsNotFallback(t *testing.T) {
	for _, tc := range []struct {
		body string
		chat bool
	}{
		{`{"error":"This model's maximum context length is 2048 tokens"}`, false},
		{`{"error":"suffix is not currently supported"}`, true},
		{`{"error":"prompt plus suffix exceeds the context length of 4096 tokens"}`, false},
		{`{"error":"registry.ollama.ai/library/llama3 does not support insert"}`, true},
	} {
		fim := newFIMServer(t, "c")
		fim.status, fim.body = http.StatusBadRequest, tc.body
		r := completionRig(t, fim.URL, "")
		noHold(t)
		id := r.open()
		res := r.cl.call("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "x"})
		fim.mu.Lock()
		paths := fmt.Sprint(fim.paths)
		fim.mu.Unlock()
		if chat := strings.Contains(paths, "chat"); chat != tc.chat || (!tc.chat && res.Error == nil) {
			t.Fatalf("%s: paths %s, reply %+v %s", tc.body, paths, res.Error, res.Result)
		}
	}
}

// A stored secret or a token straddling the cut is withheld whole: no
// fragment of it is sent.
func TestStudioCompletionsWithholdAcrossTheCut(t *testing.T) {
	fim := newFIMServer(t, "ok")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	secret := "stored-secret-value-0123456789abcdef"
	store := filepath.Join(os.Getenv("HOME"), ".abhed", "secrets.json")
	if err := os.WriteFile(store, []byte(`{"FAKE_TOKEN":"`+secret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ABHED_SECRETS_FILE", store)
	id := r.open()
	tok := "ghp_" + strings.Repeat("Z", 40)
	var res completeItems
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go",
		"prefix": secret + strings.Repeat("x", completePrefix-20), "suffix": strings.Repeat("y", completeSuffix-20) + tok,
		"context": []map[string]string{{"uri": "file://" + r.ws + "/b.go", "text": strings.Repeat("z", completeFileBytes-10) + tok}}}, &res)
	seen := fim.requests()
	if len(seen) != 1 {
		t.Fatalf("%d requests", len(seen))
	}
	sent := fmt.Sprint(seen[0]["prompt"], seen[0]["suffix"])
	if strings.Contains(sent, secret[len(secret)-12:]) || strings.Contains(sent, "ghp_ZZZZ") {
		t.Fatalf("a fragment of a secret was sent: %.80q ... %.80q", sent, sent[len(sent)-80:])
	}
}

// A reply carrying a secret is dropped, not edited into the code.
func TestStudioCompletionsDropASecretReply(t *testing.T) {
	fim := newFIMServer(t, "key := \"ghp_"+strings.Repeat("Q", 40)+"\"")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	id := r.open()
	var res completeItems
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "x"}, &res)
	if len(res.Items) != 0 {
		t.Fatalf("a reply with a token was offered: %+v", res)
	}
}

// A record that cannot be written stops the send; once it can, the next
// request records completion.enabled and goes.
func TestStudioCompletionsRecordFailureSendsNothing(t *testing.T) {
	fim := newFIMServer(t, "ok")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	id := r.open()
	rec := r.cl.conn.session(id).parts.Loop.Recorder
	rec.Gate = func() error { return errors.New("the record is gone") }
	var res completeItems
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "x"}, &res)
	if len(fim.requests()) != 0 || res.Refused == "" {
		t.Fatalf("code went out with no record: %+v", res)
	}
	rec.Gate = nil
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "x"}, &res)
	r.cl.call("session/close", map[string]any{"sessionId": id})
	enabled, _ := r.recorded(id, agent.EvCompletionEnabled)
	if len(fim.requests()) != 1 || len(enabled) != 1 || len(res.Items) != 1 {
		t.Fatalf("after the record came back: %d requests, %d records, %+v", len(fim.requests()), len(enabled), res)
	}
}

// A request that reaches the send after its session closed is not sent.
func TestCompletionNotSentAfterClose(t *testing.T) {
	k := &acpCompleter{closed: true}
	if why := (&acpConn{}).completeEnabled(nil, k, config.ProviderConfig{}, "fim"); why == "" {
		t.Fatal("a closed session's completion would be sent")
	}
}

// Under local_only no adapter follows a redirect off this machine; without
// it the redirect is followed as any is.
func TestStudioCompletionsRedirectsUnderLocalOnly(t *testing.T) {
	for _, tc := range []struct {
		name, typ, extra string
		follows          bool
	}{
		{"openai-compatible local_only", "openai-compatible", `,"completion":{"local_only":true}`, false},
		{"openai-compatible", "openai-compatible", "", true},
		{"anthropic local_only", "anthropic", `,"completion":{"local_only":true}`, false},
		{"gemini local_only", "gemini", `,"completion":{"local_only":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var far []string
			dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				far = append(far, r.URL.Path)
				mu.Unlock()
				http.Error(w, "nope", http.StatusInternalServerError)
			}))
			t.Cleanup(dst.Close)
			// localhost. reaches loopback but is not counted as this machine.
			to := strings.Replace(dst.URL, "127.0.0.1", "localhost.", 1)
			red := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, to+r.URL.Path, http.StatusTemporaryRedirect)
			}))
			t.Cleanup(red.Close)
			r := completionRig(t, red.URL, tc.extra)
			noHold(t)
			if tc.typ != "openai-compatible" {
				cfg := filepath.Join(os.Getenv("HOME"), ".abhed", "config.json")
				b, _ := os.ReadFile(cfg)
				s := strings.Replace(string(b), `"type":"openai-compatible","base_url":"`+red.URL+`/v1"`, `"type":"`+tc.typ+`","api_key":"k","base_url":"`+red.URL+`"`, 1)
				if err := os.WriteFile(cfg, []byte(s), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			id := r.open()
			res := r.cl.call("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "CODE"})
			mu.Lock()
			defer mu.Unlock()
			if followed := len(far) > 0; followed != tc.follows {
				t.Fatalf("far host got %v; reply %+v %s", far, res.Error, res.Result)
			}
			if len(far) > 1 {
				t.Fatalf("a completion was sent %d times: %v", len(far), far)
			}
		})
	}
}

// A secret the cursor splits is withheld whole: neither side keeps part of it.
func TestStudioCompletionsWithholdAcrossTheCursor(t *testing.T) {
	fim := newFIMServer(t, "ok")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	secret := "stored-secret-value-0123456789abcdef"
	store := filepath.Join(os.Getenv("HOME"), ".abhed", "secrets.json")
	if err := os.WriteFile(store, []byte(`{"FAKE_TOKEN":"`+secret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ABHED_SECRETS_FILE", store)
	id := r.open()
	tok := "ghp_" + strings.Repeat("Z", 40)
	for _, tc := range []struct{ prefix, suffix string }{
		{"k := \"" + secret[:18], secret[18:] + "\"\nnext()"},
		{"t := \"" + tok[:22], tok[22:] + "\"\nnext()"},
		{"k := \"stored-secret-valué-" + "x", "\"\nnext()"}, // no secret: sent as written
	} {
		var res completeItems
		r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": tc.prefix, "suffix": tc.suffix}, &res)
	}
	seen := fim.requests()
	if len(seen) != 3 {
		t.Fatalf("%d requests", len(seen))
	}
	for i, req := range seen[:2] {
		p, sfx := fmt.Sprint(req["prompt"]), fmt.Sprint(req["suffix"])
		for _, frag := range []string{"stored-secret", "0123456789", "ghp_", "ZZZZZZ"} {
			if strings.Contains(p, frag) || strings.Contains(sfx, frag) {
				t.Fatalf("request %d sent %q of a secret split at the cursor: %q | %q", i, frag, p, sfx)
			}
		}
		if !strings.HasSuffix(sfx, "\"\nnext()") || !strings.Contains(p+sfx, "[secret") {
			t.Fatalf("request %d: %q | %q", i, p, sfx)
		}
	}
	if p, sfx := fmt.Sprint(seen[2]["prompt"]), fmt.Sprint(seen[2]["suffix"]); p != "k := \"stored-secret-valué-x" || sfx != "\"\nnext()" {
		t.Fatalf("text with no secret changed: %q | %q", p, sfx)
	}
}

// The cursor stays where it is when only one side shows a secret, and that
// side's secret is still withheld.
func TestWithheldAroundKeepsTheCursor(t *testing.T) {
	clean := func(s string) string { out, _ := secretfiles.RedactContent(s); return out }
	for _, tc := range []struct{ prefix, suffix, wantP, wantS string }{
		{"a", "sk-abcdefghijklmnopqrstuvwx rest()", "a", "[secret] rest()"},
		{"    cfg.", "SECRET_KEY = \"abcdefgh12\"\nnext()", "    cfg.", "SECRET_KEY = \"[secret]\"\nnext()"},
		{"x := 1", "\ny := 2", "x := 1", "\ny := 2"},
		{"t := \"ghp_" + strings.Repeat("Z", 18), strings.Repeat("Z", 22) + "\"", "t := \"[secret]", "\""},
	} {
		p, s := withheldAround(clean, tc.prefix, tc.suffix)
		if p != tc.wantP || s != tc.wantS {
			t.Errorf("%q | %q -> %q | %q; want %q | %q", tc.prefix, tc.suffix, p, s, tc.wantP, tc.wantS)
		}
	}
}
