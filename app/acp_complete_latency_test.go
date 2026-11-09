package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// latencyFIM is a local completion model that takes time the way a real one
// does: a cold load before its first answer, then a token every perToken,
// streamed when asked to stream. It never answers more than tokens.
type latencyFIM struct {
	*httptest.Server
	load, perToken time.Duration
	tokens         []string

	mu     sync.Mutex
	bodies []map[string]any
	loaded bool
	ended  atomic.Int32 // requests the client ended before the answer did
}

func newLatencyFIM(t *testing.T, load, perToken time.Duration, tokens ...string) *latencyFIM {
	t.Helper()
	f := &latencyFIM{load: load, perToken: perToken, tokens: tokens}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions" {
			http.Error(w, "not here", http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		f.mu.Lock()
		f.bodies = append(f.bodies, m)
		wait := time.Duration(0)
		if !f.loaded {
			wait, f.loaded = f.load, true
		}
		f.mu.Unlock()
		pause := func(d time.Duration) bool {
			select {
			case <-r.Context().Done():
				f.ended.Add(1)
				return false
			case <-time.After(d):
				return true
			}
		}
		if !pause(wait) {
			return
		}
		n := len(f.tokens)
		if mt, ok := m["max_tokens"].(float64); ok && int(mt) < n {
			n = int(mt)
		}
		if m["stream"] != true {
			if !pause(time.Duration(n) * f.perToken) {
				return
			}
			out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"text": strings.Join(f.tokens[:n], "")}}})
			_, _ = w.Write(out)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, tok := range f.tokens[:n] {
			if !pause(f.perToken) {
				return
			}
			out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"text": tok}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", out)
			w.(http.Flusher).Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *latencyFIM) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.bodies...)
}

// pythonBody is a model writing a whole function body, a token a chunk.
var pythonBody = append([]string{"\"\"\"Return", " the", " number", " of", " words.\"\"\"\n"},
	strings.Split(strings.Repeat("    words = text.split()\n    # count them\n", 20), " ")...)

// A suggestion is one logical block, and the model is stopped once the block
// is whole: against a stub writing a token every 25 ms, the docstring line
// comes back in well under the time the whole body takes, and the stub sees
// the request end. The summary records the answer times, and no code.
func TestStudioCompletionsCapAtOneBlock(t *testing.T) {
	fim := newLatencyFIM(t, 0, 25*time.Millisecond, pythonBody...)
	r := completionRig(t, fim.URL, "")
	noHold(t)
	id := r.open()
	var res completeItems
	started := time.Now()
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/text.py", "language": "python",
		"prefix": "def word_count(text):\n    ", "suffix": "\n"}, &res)
	took := time.Since(started)
	if len(res.Items) != 1 || res.Items[0]["text"] != `"""Return the number of words."""` {
		t.Fatalf("items %+v", res)
	}
	whole := time.Duration(min(len(pythonBody), completeMaxTokens)) * 25 * time.Millisecond
	if took > whole/3 {
		t.Fatalf("one block took %v; the whole answer takes %v", took, whole)
	}
	body := fim.requests()[0]
	stops, _ := body["stop"].([]any)
	if body["stream"] != true || len(stops) == 0 || stops[0] != "<|endoftext|>" {
		t.Fatalf("request %v", body)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fim.ended.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fim.ended.Load() == 0 {
		t.Fatal("the model was never stopped")
	}
	r.cl.call("session/close", map[string]any{"sessionId": id})
	sum, _ := r.recorded(id, agent.EvCompletionSummary)
	if len(sum) != 1 || sum[0]["latency_p50_ms"] == nil || sum[0]["latency_max_ms"].(float64) < 100 {
		t.Fatalf("completion.summary %v", sum)
	}
	if raw, _ := json.Marshal(sum[0]); strings.Contains(string(raw), "word") {
		t.Fatalf("the summary holds code: %s", raw)
	}
	t.Logf("stub at 25 ms a token: one block answered in %v (whole body %v)", took.Round(time.Millisecond), whole)
}

// A burst of keystrokes sends one request: each newer one supersedes the
// one waiting out the debounce, and only what is sent spends the rate
// limit, so the last keystroke, the one that matters, is never refused.
func TestStudioCompletionsBurstSendsTheLast(t *testing.T) {
	fim := newLatencyFIM(t, 0, 10*time.Millisecond, "return", " a", " +", " b\n", "x = 1\n")
	r := completionRig(t, fim.URL, "")
	id := r.open()
	from := r.cl.mark()
	const burst = 12
	var lastSent time.Time
	for i := 1; i <= burst; i++ {
		p, _ := json.Marshal(map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.py", "language": "python",
			"prefix": "def add(a, b):\n    " + strings.Repeat("r", i), "suffix": "", "seq": i})
		lastSent = time.Now()
		r.cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf(`"k%d"`, i)), Method: "_abhed/complete", Params: p})
		time.Sleep(15 * time.Millisecond)
	}
	last := r.cl.waitFor(from, "the last answer", func(m rpcMessage) bool { return string(m.ID) == fmt.Sprintf(`"k%d"`, burst) })
	took := time.Since(lastSent)
	var res completeItems
	_ = json.Unmarshal(last.Result, &res)
	if len(res.Items) != 1 || res.Items[0]["text"] != "return a + b" {
		t.Fatalf("the last keystroke's answer: %s", last.Result)
	}
	for i := 1; i < burst; i++ {
		m := r.cl.waitFor(from, "an earlier answer", func(m rpcMessage) bool { return string(m.ID) == fmt.Sprintf(`"k%d"`, i) })
		if !strings.Contains(string(m.Result), "cancelled") {
			t.Fatalf("keystroke %d: %s", i, m.Result)
		}
	}
	if n := len(fim.requests()); n != 1 {
		t.Fatalf("%d requests reached the model; want 1", n)
	}
	t.Logf("burst of %d keystrokes 15 ms apart: 1 request sent, the last answered %v after it was typed", burst, took.Round(time.Millisecond))
}

// A warm-up loads a local model before the first completion needs it: an
// empty request of one token, no code, at most once in warmEvery; a model
// not on this machine is never warmed and nothing is sent. Against a stub
// with a 400 ms cold load, the first completion after it pays none of it.
func TestStudioCompletionsWarmUp(t *testing.T) {
	fim := newLatencyFIM(t, 400*time.Millisecond, 5*time.Millisecond, "return 1\n", "x\n")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	id := r.open()
	var warm struct {
		Warmed  bool   `json:"warmed"`
		Recent  bool   `json:"recent"`
		MS      int64  `json:"ms"`
		Skipped string `json:"skipped"`
	}
	r.cl.ok("_abhed/complete/warm", map[string]any{"sessionId": id}, &warm)
	if !warm.Warmed || warm.MS < 400 {
		t.Fatalf("warm %+v", warm)
	}
	body := fim.requests()[0]
	if body["prompt"] != "" || body["suffix"] != "" || body["max_tokens"] != float64(1) {
		t.Fatalf("the warm-up sent %v", body)
	}
	if enabled, _ := r.recorded(id, agent.EvCompletionEnabled); len(enabled) != 0 {
		t.Fatalf("a warm-up sends no code, so it records no completion.enabled: %v", enabled)
	}
	r.cl.ok("_abhed/complete/warm", map[string]any{"sessionId": id}, &warm)
	if !warm.Recent || len(fim.requests()) != 1 {
		t.Fatalf("a second warm-up at once was sent: %+v", warm)
	}
	var res completeItems
	started := time.Now()
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.py", "prefix": "def f():\n    ", "suffix": ""}, &res)
	took := time.Since(started)
	if len(res.Items) != 1 || took > 300*time.Millisecond {
		t.Fatalf("after the warm-up the completion took %v: %+v", took, res)
	}
	r.cl.call("session/close", map[string]any{"sessionId": id})
	sum, _ := r.recorded(id, agent.EvCompletionSummary)
	if len(sum) != 1 || sum[0]["warmups"] != float64(1) || sum[0]["warmup_ms"].(float64) < 400 {
		t.Fatalf("completion.summary %v", sum)
	}
	t.Logf("stub with a 400 ms cold load: warm-up %d ms, first completion after it %v", warm.MS, took.Round(time.Millisecond))

	remote := newLatencyFIM(t, 0, 0, "x")
	rr := completionRig(t, strings.Replace(remote.URL, "127.0.0.1", "abhed-remote.invalid", 1), "")
	rid := rr.open()
	rr.cl.ok("_abhed/complete/warm", map[string]any{"sessionId": rid}, &warm)
	if warm.Warmed || !strings.Contains(warm.Skipped, "not on this machine") || len(remote.requests()) != 0 {
		t.Fatalf("a remote model was warmed: %+v", warm)
	}
	off := completionRig(t, fim.URL, `,"completion":{"enabled":false}`)
	oid := off.open()
	off.cl.ok("_abhed/complete/warm", map[string]any{"sessionId": oid}, &warm)
	if warm.Warmed || !strings.Contains(warm.Skipped, "turned off") {
		t.Fatalf("warmed with completions off: %+v", warm)
	}
}

// A cancel names the request it is for: one for an older seq leaves a newer
// request going, and one for the request in flight ends it.
func TestStudioCompletionsCancelBySeq(t *testing.T) {
	fim := newFIMServer(t, "y")
	r := completionRig(t, fim.URL, "")
	noHold(t)
	id := r.open()
	fim.mu.Lock()
	fim.gate = make(chan struct{})
	fim.mu.Unlock()
	from := r.cl.mark()
	p, _ := json.Marshal(map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "a", "seq": 5})
	r.cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(`"c5"`), Method: "_abhed/complete", Params: p})
	<-fim.arrived
	r.cl.ok("_abhed/complete/cancel", map[string]any{"sessionId": id, "seq": 4}, nil)
	select {
	case <-time.After(150 * time.Millisecond):
	case <-func() chan struct{} {
		ch := make(chan struct{})
		go func() { r.cl.waitFor(from, "c5", func(m rpcMessage) bool { return string(m.ID) == `"c5"` }); close(ch) }()
		return ch
	}():
		t.Fatal("a cancel for seq 4 ended seq 5")
	}
	r.cl.ok("_abhed/complete/cancel", map[string]any{"sessionId": id, "seq": 5}, nil)
	m := r.cl.waitFor(from, "c5", func(m rpcMessage) bool { return string(m.ID) == `"c5"` })
	if !strings.Contains(string(m.Result), "cancelled") {
		t.Fatalf("seq 5 after its cancel: %s", m.Result)
	}
	close(fim.gate)
}

// A warm-up belongs to its session: closing the session ends it, and the
// model is not left loading for nobody. A second session on the same
// connection does not load the model again.
func TestStudioCompletionsWarmUpEndsWithTheSession(t *testing.T) {
	fim := newLatencyFIM(t, 3*time.Second, 0, "x\n")
	r := completionRig(t, fim.URL, "")
	id := r.open()
	from := r.cl.mark()
	p, _ := json.Marshal(map[string]any{"sessionId": id})
	r.cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(`"w1"`), Method: "_abhed/complete/warm", Params: p})
	deadline := time.Now().Add(2 * time.Second)
	for len(fim.requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	second := r.open()
	var warm struct {
		Recent bool `json:"recent"`
	}
	r.cl.ok("_abhed/complete/warm", map[string]any{"sessionId": second}, &warm)
	if !warm.Recent || len(fim.requests()) != 1 {
		t.Fatalf("a second session loaded the model again: %+v, %d requests", warm, len(fim.requests()))
	}
	closed := time.Now()
	r.cl.call("session/close", map[string]any{"sessionId": id})
	m := r.cl.waitFor(from, "the warm-up's answer", func(m rpcMessage) bool { return string(m.ID) == `"w1"` })
	if took := time.Since(closed); took > time.Second || strings.Contains(string(m.Result), `"warmed":true`) {
		t.Fatalf("the warm-up outlived its session by %v: %s", took, m.Result)
	}
	for fim.ended.Load() == 0 && time.Now().Before(deadline.Add(time.Second)) {
		time.Sleep(10 * time.Millisecond)
	}
	if fim.ended.Load() == 0 {
		t.Fatal("the model was never told")
	}
}
