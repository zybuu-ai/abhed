package app

import (
	"context"
	"encoding/json"
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

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
)

// editorModel is one model server for completions and next edits, as a
// local server with one slot is: each path can be held, and what arrives,
// ends and is cut off is kept in order.
type editorModel struct {
	*httptest.Server
	mu       sync.Mutex
	log      []string
	chatMax  []float64
	suffixes []any
	gates    map[string]chan struct{}
	arrived  chan string
	fim      string
	chat     string
	finish   string // the chat answer's finish_reason; "stop" when empty
}

func newEditorModel(t *testing.T, fim, chat string) *editorModel {
	t.Helper()
	m := &editorModel{gates: map[string]chan struct{}{}, arrived: make(chan string, 64), fim: fim, chat: chat}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		kind := "fim"
		if r.URL.Path == "/v1/chat/completions" {
			kind = "chat"
		}
		m.mu.Lock()
		m.log = append(m.log, kind+" start")
		if kind == "chat" {
			mt, _ := body["max_tokens"].(float64)
			m.chatMax = append(m.chatMax, mt)
		} else {
			m.suffixes = append(m.suffixes, body["suffix"])
		}
		gate := m.gates[kind]
		m.mu.Unlock()
		m.arrived <- kind
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				m.note(kind + " cut off")
				return
			}
		}
		if kind == "chat" {
			finish := m.finish
			if finish == "" {
				finish = "stop"
			}
			out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": m.chat}, "finish_reason": finish}}})
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", out)
		} else {
			out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"text": m.fim}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 4}})
			_, _ = w.Write(out)
		}
		m.note(kind + " done")
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *editorModel) note(s string) {
	m.mu.Lock()
	m.log = append(m.log, s)
	m.mu.Unlock()
}

func (m *editorModel) hold(kind string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan struct{})
	m.gates[kind] = ch
	return ch
}

func (m *editorModel) events() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.log...)
}

func waitArrival(t *testing.T, m *editorModel, kind string) {
	t.Helper()
	for {
		select {
		case k := <-m.arrived:
			if k == kind {
				return
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no %s request arrived", kind)
		}
	}
}

func waitLogged(t *testing.T, m *editorModel, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range m.events() {
			if e == what {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never logged %q: %v", what, m.events())
}

const feelWindow = "def chunk(text, size, overlap):\n    chunks = []\n    step = size - overlap\n    return chunks\n"

func nextEditParams(id, uri string) map[string]any {
	return map[string]any{"sessionId": id, "uri": uri, "language": "python",
		"cursor": map[string]int{"line": 2, "character": 4}, "window": map[string]any{"startLine": 0, "text": feelWindow}}
}

func send(r *studioRig, id, method string, params any) {
	b, _ := json.Marshal(params)
	r.cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(`"` + id + `"`), Method: method, Params: b})
}

// A completion asked for while a next-edit call is on the server cancels
// that call, and is answered; the next edit says a completion went first.
func TestStudioCompletionPreemptsNextEdit(t *testing.T) {
	m := newEditorModel(t, "return chunks", strings.Replace(feelWindow, "chunks = []", "chunks = list()", 1))
	r := completionRig(t, m.URL, "")
	noHold(t)
	id := r.open()
	uri := "file://" + r.ws + "/text.py"
	m.hold("chat")
	from := r.cl.mark()
	send(r, "n1", "_abhed/nextEdit", nextEditParams(id, uri))
	waitArrival(t, m, "chat")
	var res completeItems
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": uri, "language": "python", "prefix": "def f():\n    ", "suffix": "\n"}, &res)
	if len(res.Items) != 1 || res.Items[0]["text"] != "return chunks" {
		t.Fatalf("completion %+v", res)
	}
	n1 := r.cl.waitFor(from, "the next edit's answer", func(m rpcMessage) bool { return string(m.ID) == `"n1"` })
	if !strings.Contains(string(n1.Result), "a completion went first") {
		t.Fatalf("next edit: %s", n1.Result)
	}
	waitLogged(t, m, "chat cut off")
}

// A next edit asked for while a completion is on the server waits for it:
// the model is not asked until the completion is answered.
func TestStudioNextEditWaitsForCompletions(t *testing.T) {
	m := newEditorModel(t, "return chunks", strings.Replace(feelWindow, "chunks = []", "chunks = list()", 1))
	r := completionRig(t, m.URL, "")
	noHold(t)
	id := r.open()
	uri := "file://" + r.ws + "/text.py"
	gate := m.hold("fim")
	from := r.cl.mark()
	send(r, "c1", "_abhed/complete", map[string]any{"sessionId": id, "uri": uri, "language": "python", "prefix": "def f():\n    ", "suffix": "\n"})
	waitArrival(t, m, "fim")
	send(r, "n1", "_abhed/nextEdit", nextEditParams(id, uri))
	time.Sleep(300 * time.Millisecond)
	for _, e := range m.events() {
		if strings.HasPrefix(e, "chat") {
			t.Fatalf("the next edit went before the completion: %v", m.events())
		}
	}
	close(gate)
	r.cl.waitFor(from, "the completion", func(m rpcMessage) bool { return string(m.ID) == `"c1"` })
	n1 := r.cl.waitFor(from, "the next edit", func(m rpcMessage) bool { return string(m.ID) == `"n1"` })
	if !strings.Contains(string(n1.Result), "chunks = list()") {
		t.Fatalf("next edit: %s", n1.Result)
	}
	ev := m.events()
	if len(ev) != 4 || ev[0] != "fim start" || ev[1] != "fim done" || ev[2] != "chat start" {
		t.Fatalf("order %v", ev)
	}
	// Its answer is bounded by the window it rewrites, not the 1,024 tokens it had.
	m.mu.Lock()
	max := m.chatMax
	m.mu.Unlock()
	if len(max) != 1 || max[0] != float64(nextEditTokens(feelWindow)) || max[0] >= nextEditMaxTokens {
		t.Fatalf("max_tokens %v", max)
	}
}

// A rename is finished from the file at once, however busy the server is.
func TestStudioRenameDoesNotWaitForTheModel(t *testing.T) {
	m := newEditorModel(t, "x", "never asked")
	r := completionRig(t, m.URL, "")
	noHold(t)
	id := r.open()
	uri := "file://" + r.ws + "/text.py"
	gate := m.hold("fim")
	defer close(gate)
	send(r, "c1", "_abhed/complete", map[string]any{"sessionId": id, "uri": uri, "language": "python", "prefix": "def f():\n    ", "suffix": "\n"})
	waitArrival(t, m, "fim")
	window := "def chunk(text, size, overlap):\n    chunks = []\n    stride = size - overlap\n    for start in range(0, len(text), step):\n        chunks.append(text[start:start + size])\n    return chunks, step\n"
	var res nextEdits
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.cl.ok("_abhed/nextEdit", map[string]any{"sessionId": id, "uri": uri, "language": "python",
			"rename": map[string]any{"from": "step", "to": "stride", "at": map[string]int{"line": 2, "character": 4}},
			"cursor": map[string]int{"line": 2, "character": 10}, "window": map[string]any{"startLine": 0, "text": window}}, &res)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the rename waited for the completion")
	}
	if len(res.Edits) != 1 || !strings.Contains(res.Edits[0].Text, "len(text), stride)") || !strings.Contains(res.Edits[0].Text, "return chunks, stride") {
		t.Fatalf("%+v", res)
	}
}

// Studio cancels the next edit in flight on each keystroke.
func TestStudioNextEditCancel(t *testing.T) {
	m := newEditorModel(t, "x", feelWindow)
	r := completionRig(t, m.URL, "")
	noHold(t)
	id := r.open()
	uri := "file://" + r.ws + "/text.py"
	m.hold("chat")
	from := r.cl.mark()
	send(r, "n1", "_abhed/nextEdit", nextEditParams(id, uri))
	waitArrival(t, m, "chat")
	asked := time.Now()
	r.cl.ok("_abhed/nextEdit/cancel", map[string]any{"sessionId": id}, nil)
	n1 := r.cl.waitFor(from, "the next edit's answer", func(m rpcMessage) bool { return string(m.ID) == `"n1"` })
	// At once, not at the request's own deadline.
	if !strings.Contains(string(n1.Result), "cancelled") || time.Since(asked) > completeTimeout/2 {
		t.Fatalf("next edit after %v: %s", time.Since(asked), n1.Result)
	}
	waitLogged(t, m, "chat cut off")
}

// At the end of a file the request still carries a suffix, so a coder
// model fills in the middle; and a suggestion that writes the rest of the
// cursor's line again is not inserted twice.
func TestStudioCompletionsAtTheEndAndMidLine(t *testing.T) {
	m := newEditorModel(t, " = size - overlap\n    for start in range(0, len(text), stride):\n", "")
	r := completionRig(t, m.URL, "")
	noHold(t)
	id := r.open()
	uri := "file://" + r.ws + "/text.py"
	var res completeItems
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": uri, "language": "python",
		"prefix": "def chunk(text, size, overlap):\n    stride", "suffix": " = size - overlap\n    for start in range(0, len(text), step):\n"}, &res)
	if len(res.Items) != 0 {
		t.Fatalf("the line's rest was offered again: %+v", res)
	}
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": uri, "language": "python",
		"prefix": "def word_count(text):\n    ", "suffix": ""}, &res)
	m.mu.Lock()
	suffixes := m.suffixes
	m.mu.Unlock()
	if len(suffixes) != 2 || suffixes[1] != "\n" {
		t.Fatalf("suffixes sent %q", suffixes)
	}
}

// A next edit cut at its token bound is dropped: the lines the model never
// reached are not offered as deletions (architect review r1).
func TestStudioNextEditCutShortIsNotADeletion(t *testing.T) {
	m := newEditorModel(t, "x", "def chunk(text, size, overlap):\n    chunks = list()\n")
	m.finish = "length"
	r := completionRig(t, m.URL, "")
	noHold(t)
	id := r.open()
	var res nextEdits
	r.cl.ok("_abhed/nextEdit", nextEditParams(id, "file://"+r.ws+"/text.py"), &res)
	if len(res.Edits) != 0 || !strings.Contains(res.Dropped, "cut short") {
		t.Fatalf("%+v", res)
	}
}

// Completions go first only on a server that queues requests: one on this
// machine, ollama, or one held to max_concurrent_requests; a hosted server
// serves them side by side, so nothing waits and nothing is cancelled there.
func TestEditorServerQueues(t *testing.T) {
	for _, tc := range []struct {
		p    config.ProviderConfig
		want string
	}{
		{config.ProviderConfig{Type: "openai-compatible", BaseURL: "http://127.0.0.1:11434/v1/"}, "http://127.0.0.1:11434/v1"},
		{config.ProviderConfig{Type: "ollama", BaseURL: "HTTP://Box.lan:80/v1"}, "http://box.lan/v1"},
		{config.ProviderConfig{Type: "vllm", BaseURL: "https://gpu.example/v1", MaxConcurrentRequests: 2}, "https://gpu.example/v1"},
		{config.ProviderConfig{Type: "openai-compatible", BaseURL: "https://api.example/v1"}, ""},
		{config.ProviderConfig{Type: "anthropic"}, ""},
		// A local type left at its default endpoint (architect review r2): pooled, keyed as its adapter resolves it.
		{config.ProviderConfig{Type: "ollama", Model: "qwen2.5-coder:7b"}, model.ServerKey(model.LocalDefaultURL("ollama"))},
		{config.ProviderConfig{Type: "vllm"}, "http://127.0.0.1:8000/v1"},
		{config.ProviderConfig{Type: "openai-compatible"}, ""},
	} {
		if got := editorServer(tc.p); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.p, got, tc.want)
		}
	}
}

// A next edit cancelled while it still waits for a one-slot server's request
// slot was never sent, and is not charged; one cancelled after it was sent
// is (architect review r2).
func TestStudioNextEditQueuedAndCancelledIsNotCharged(t *testing.T) {
	m := newEditorModel(t, "x", feelWindow)
	m.hold("chat")
	sm := newScriptModel(t)
	acpModelsEnv(t, `{"model":{"default":"m","completion":"coder","providers":{"m":`+stubProvider(sm.URL, "m-1")+
		`,"coder":{"type":"ollama","base_url":"`+m.URL+`/v1","model":"qwen2.5-coder"}}}}`)
	ws := t.TempDir()
	if real, err := filepath.EvalSymlinks(ws); err == nil {
		ws = real
	}
	r := &studioRig{t: t, ws: ws, home: os.Getenv("HOME"), model: sm}
	r.cl = newStudioClient(t, ws, true)
	r.cl.ok("initialize", map[string]any{"protocolVersion": 1}, nil)
	noHold(t)
	uri := "file://" + r.ws + "/text.py"

	// Another call holds the server's one slot.
	holder, err := model.New(model.Spec{Type: "ollama", BaseURL: m.URL + "/v1", Model: "qwen2.5-coder"})
	if err != nil {
		t.Fatal(err)
	}
	hctx, hcancel := context.WithCancel(context.Background())
	defer hcancel()
	go func() {
		if ch, err := holder.Complete(hctx, model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}}}); err == nil {
			for range ch {
			}
		}
	}()
	waitArrival(t, m, "chat")

	charged := func(id string, cancel func()) map[string]any {
		t.Helper()
		from := r.cl.mark()
		send(r, "n-"+id, "_abhed/nextEdit", nextEditParams(id, uri))
		cancel()
		r.cl.ok("_abhed/nextEdit/cancel", map[string]any{"sessionId": id}, nil)
		ans := r.cl.waitFor(from, "the next edit", func(msg rpcMessage) bool { return string(msg.ID) == `"n-`+id+`"` })
		if !strings.Contains(string(ans.Result), "cancelled") {
			t.Fatalf("next edit: %s", ans.Result)
		}
		r.cl.call("session/close", map[string]any{"sessionId": id})
		sum, _ := r.recorded(id, agent.EvNextEditSummary)
		if len(sum) != 1 || sum[0]["cancelled"] != float64(1) {
			t.Fatalf("next_edit.summary %v", sum)
		}
		return sum[0]
	}
	// Queued behind the holder, then cancelled: nothing went, nothing is charged.
	queued := r.open()
	sum := charged(queued, func() {
		for deadline := time.Now().Add(10 * time.Second); model.Waiting(m.URL+"/v1", "qwen2.5-coder") == 0; {
			if time.Now().After(deadline) {
				t.Fatal("the next edit never queued for the slot")
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	if sum["tokens_in"] != float64(0) || sum["tokens_out"] != float64(0) || sum["estimated"] != nil || sum["cost_usd"] != nil {
		t.Fatalf("a call that never went was charged: %v", sum)
	}
	// The slot free, the call goes, and is cancelled at the server: charged by estimate.
	hcancel()
	sent := r.open()
	sum = charged(sent, func() { waitArrival(t, m, "chat") })
	if tin, _ := sum["tokens_in"].(float64); tin <= 0 || sum["estimated"] != float64(1) {
		t.Fatalf("a call that went was not charged: %v", sum)
	}
}
