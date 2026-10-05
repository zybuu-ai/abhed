package app

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store/local"
)

// The Studio contract's tests drive the real engine through a scripted ACP
// client: a model endpoint that answers from a script, the local record in a
// home of the test's own, and the sandbox the configuration asks for.

// studioClient is an editor on the other end of the pipes. It keeps every
// message, so a notification that arrived during a request is still there.
type studioClient struct {
	t      *testing.T
	conn   *acpConn
	in     io.Writer
	mu     sync.Mutex
	cond   *sync.Cond
	seen   []rpcMessage
	nextID atomic.Int64
	// answer answers the engine's requests: permission asks and confirms.
	answerMu sync.Mutex
	answer   func(method string, params json.RawMessage) any
}

func newStudioClient(t *testing.T, ws string, durable bool, trust ...config.TrustChoice) *studioClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &acpConn{out: outW, version: "test", build: acpBuild{Version: "1.2.3", Edition: "ce", Commit: "abc"}, base: ws,
		sessions: map[string]*acpSession{}, pending: map[int64]chan rpcMessage{}}
	if len(trust) > 0 {
		c.trust = trust[0]
	}
	if durable {
		c.useRecord(ws, "")
	}
	cl := &studioClient{t: t, conn: c, in: inW}
	cl.cond = sync.NewCond(&cl.mu)
	cl.answer = func(string, json.RawMessage) any {
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
	}
	go func() { _ = c.serve(inR) }()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
		for sc.Scan() {
			var m rpcMessage
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			if m.Method != "" && m.ID != nil {
				cl.answerMu.Lock()
				answer := cl.answer
				cl.answerMu.Unlock()
				// Answered aside, as a dialog is, so a slow one holds up nothing else.
				go func(m rpcMessage) {
					res, _ := json.Marshal(answer(m.Method, m.Params))
					cl.write(rpcMessage{JSONRPC: "2.0", ID: m.ID, Result: res})
				}(m)
			}
			cl.mu.Lock()
			cl.seen = append(cl.seen, m)
			cl.cond.Broadcast()
			cl.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		c.closeAll()
		c.closeRecord()
		_ = inW.Close()
		_ = outR.Close()
	})
	return cl
}

func (cl *studioClient) answering(f func(method string, params json.RawMessage) any) {
	cl.answerMu.Lock()
	cl.answer = f
	cl.answerMu.Unlock()
}

func (cl *studioClient) write(m rpcMessage) {
	b, _ := json.Marshal(m)
	_, _ = cl.in.Write(append(b, '\n'))
}

// waitFor returns the first message from index from on that matches.
func (cl *studioClient) waitFor(from int, what string, match func(rpcMessage) bool) rpcMessage {
	cl.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	timer := time.AfterFunc(20*time.Second, func() {
		cl.mu.Lock()
		cl.cond.Broadcast()
		cl.mu.Unlock()
	})
	defer timer.Stop()
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for {
		for i := from; i < len(cl.seen); i++ {
			if match(cl.seen[i]) {
				return cl.seen[i]
			}
		}
		from = len(cl.seen)
		if time.Now().After(deadline) {
			cl.t.Fatalf("no %s arrived", what)
		}
		cl.cond.Wait()
	}
}

// mark is where the next message will be kept.
func (cl *studioClient) mark() int {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return len(cl.seen)
}

// since are the messages kept from index from on.
func (cl *studioClient) since(from int) []rpcMessage {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return append([]rpcMessage(nil), cl.seen[from:]...)
}

// call sends a request and waits for its reply.
func (cl *studioClient) call(method string, params any) rpcMessage {
	cl.t.Helper()
	id := strconv.FormatInt(cl.nextID.Add(1), 10)
	raw, _ := json.Marshal(params)
	from := cl.mark()
	cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: raw})
	m := cl.waitFor(from, "reply to "+method, func(m rpcMessage) bool { return m.Method == "" && string(m.ID) == id })
	return m
}

// ok sends a request that must succeed and decodes its result.
func (cl *studioClient) ok(method string, params any, into any) {
	cl.t.Helper()
	m := cl.call(method, params)
	if m.Error != nil {
		cl.t.Fatalf("%s: %d %s", method, m.Error.Code, m.Error.Message)
	}
	if into != nil {
		if err := json.Unmarshal(m.Result, into); err != nil {
			cl.t.Fatalf("%s: %v in %s", method, err, m.Result)
		}
	}
}

// refused sends a request that must fail with code, and returns its message.
func (cl *studioClient) refused(code int, method string, params any) string {
	cl.t.Helper()
	m := cl.call(method, params)
	if m.Error == nil || m.Error.Code != code {
		cl.t.Fatalf("%s: want error %d, got %+v %s", method, code, m.Error, m.Result)
	}
	return m.Error.Message
}

// updates are the session/update payloads among msgs.
func updates(msgs []rpcMessage) []map[string]any {
	var out []map[string]any
	for _, m := range msgs {
		if m.Method != "session/update" {
			continue
		}
		var p struct {
			Update map[string]any `json:"update"`
		}
		_ = json.Unmarshal(m.Params, &p)
		out = append(out, p.Update)
	}
	return out
}

func meta(v map[string]any) map[string]any {
	m, _ := v["_meta"].(map[string]any)
	block, _ := m[acpMetaKey].(map[string]any)
	return block
}

// scriptModel is a model endpoint that answers each request with the next
// frame of its script, and then with "done".
type scriptModel struct {
	*httptest.Server
	mu     sync.Mutex
	frames []string
	n      int
	// suggestion answers the next-prompt call; "" answers NONE.
	suggestion string
}

func newScriptModel(t *testing.T, frames ...string) *scriptModel {
	t.Helper()
	m := &scriptModel{frames: frames}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		frame := `{"choices":[{"delta":{"content":"done"}}]}`
		if bytes.Contains(body, []byte("predict the next message")) {
			// The next-prompt call takes no frame of the script.
			frame = say(cmp.Or(m.suggestion, "NONE"))
		} else {
			if m.n < len(m.frames) {
				frame = m.frames[m.n]
			}
			m.n++
		}
		m.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", frame)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(m.Close)
	return m
}

// script replaces what the model says next.
func (m *scriptModel) script(frames ...string) {
	m.mu.Lock()
	m.frames, m.n = frames, 0
	m.mu.Unlock()
}

// say is a frame of plain text.
func say(text string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}}}})
	return string(b)
}

// call is a frame that calls one tool with any arguments.
func callTool(id, name string, args map[string]any) string {
	a, _ := json.Marshal(args)
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{"index": 0, "id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": string(a)}}}}}}})
	return string(b)
}

// studioRig is the engine, its model and a workspace, under a home of the test's own.
type studioRig struct {
	t     *testing.T
	cl    *studioClient
	ws    string
	home  string
	model *scriptModel
}

// newStudioRig starts an engine whose user configuration is extra merged
// over a model that follows the script.
func newStudioRig(t *testing.T, extra string, frames ...string) *studioRig {
	t.Helper()
	m := newScriptModel(t, frames...)
	userCfg := `{"model":{"default":"m","providers":{"m":` + stubProvider(m.URL, "m-1") + `}}` + extra + `}`
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

// open starts a session and returns its id.
func (r *studioRig) open() string {
	r.t.Helper()
	var res struct {
		SessionID string `json:"sessionId"`
	}
	r.cl.ok("session/new", map[string]any{"cwd": r.ws, "mcpServers": []any{}}, &res)
	return res.SessionID
}

// prompt runs one prompt and returns its stop reason and the updates it sent.
func (r *studioRig) prompt(id, text string) (string, []map[string]any) {
	r.t.Helper()
	from := r.cl.mark()
	var res struct {
		StopReason string `json:"stopReason"`
	}
	r.cl.ok("session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]any{"type": "text", "text": text}}}, &res)
	return res.StopReason, updates(r.cl.since(from))
}

// events are the session's events as the record holds them.
func (r *studioRig) events(id string) []agent.Event {
	r.t.Helper()
	rec, err := r.cl.conn.record()
	if err != nil {
		r.t.Fatal(err)
	}
	evs, err := rec.Events(id)
	if err != nil {
		r.t.Fatal(err)
	}
	return evs
}

// recorded returns the payloads of the session's events of type t, and their actors.
func (r *studioRig) recorded(id string, t agent.EventType) (payloads []map[string]any, actors []agent.Actor) {
	for _, ev := range r.events(id) {
		if ev.Type == t {
			var p map[string]any
			_ = json.Unmarshal(ev.Payload, &p)
			payloads, actors = append(payloads, p), append(actors, ev.Actor)
		}
	}
	return payloads, actors
}

func (r *studioRig) write(rel, content string) string {
	r.t.Helper()
	p := filepath.Join(r.ws, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *studioRig) read(rel string) string {
	b, _ := os.ReadFile(filepath.Join(r.ws, filepath.FromSlash(rel)))
	return string(b)
}

// recordStore opens the rig's record directly, as another reader would.
func (r *studioRig) recordStore() *local.Store {
	rec, err := r.cl.conn.record()
	if err != nil {
		r.t.Fatal(err)
	}
	return rec
}
