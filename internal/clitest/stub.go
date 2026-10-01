package clitest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// step is one line of a Script.
type step struct {
	op   string // text, reasoning, tool, delay, stall, error, usage
	text string
	name string
	args string
	dur  time.Duration
	code int
	in   int
	out  int
	hit  int
}

// turn is one model response: the steps between blank lines.
type turn []step

// ParseScript parses the stub model's DSL. Each non-blank line is a step;
// a blank line ends a turn. Lines starting with # are comments.
func ParseScript(s Script) ([]turn, error) {
	var turns []turn
	var cur turn
	flush := func() {
		if len(cur) > 0 {
			turns = append(turns, cur)
			cur = nil
		}
	}
	for i, line := range strings.Split(string(s), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		op, rest, _ := strings.Cut(line, " ")
		rest = strings.TrimSpace(rest)
		st := step{op: op}
		var err error
		switch op {
		case "text", "reasoning":
			st.text, err = strconv.Unquote(rest)
		case "tool":
			st.name, st.args, _ = strings.Cut(rest, " ")
			st.args = strings.TrimSpace(st.args)
			if st.args == "" {
				st.args = "{}"
			}
			if !json.Valid([]byte(st.args)) {
				err = fmt.Errorf("arguments are not JSON")
			}
		case "delay", "stall":
			st.dur, err = time.ParseDuration(rest)
		case "error":
			st.code, err = strconv.Atoi(rest)
		case "usage":
			for _, kv := range strings.Fields(rest) {
				k, v, _ := strings.Cut(kv, "=")
				n, convErr := strconv.Atoi(v)
				if convErr != nil {
					err = convErr
				}
				switch k {
				case "in":
					st.in = n
				case "out":
					st.out = n
				case "cached":
					st.hit = n
				default:
					err = fmt.Errorf("unknown usage field %q", k)
				}
			}
		default:
			err = fmt.Errorf("unknown step %q", op)
		}
		if err != nil {
			return nil, fmt.Errorf("script line %d %q: %w", i+1, line, err)
		}
		cur = append(cur, st)
	}
	flush()
	return turns, nil
}

// Stub is the scripted OpenAI-compatible model. Each request takes the
// next turn of the script; past the end it answers "(end of script)".
type Stub struct {
	srv   *httptest.Server
	mu    sync.Mutex
	turns []turn
	next  int
	reqs  []Request
	delta []Delta
	// Models is what GET /models lists.
	Models []string
}

// NewStub starts a stub serving script on loopback.
func NewStub(t interface {
	Helper()
	Fatalf(string, ...any)
	Cleanup(func())
}, s Script) *Stub {
	t.Helper()
	turns, err := ParseScript(s)
	if err != nil {
		t.Fatalf("clitest: %v", err)
	}
	st := &Stub{turns: turns, Models: []string{"stub-model"}}
	st.srv = httptest.NewServer(http.HandlerFunc(st.serve))
	t.Cleanup(st.srv.Close)
	return st
}

// URL is the base URL a provider's base_url takes, ending in /v1.
func (s *Stub) URL() string { return s.srv.URL + "/v1" }

// Requests are the requests received, in order.
func (s *Stub) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// Deltas are the fragments streamed, in order.
func (s *Stub) Deltas() []Delta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Delta(nil), s.delta...)
}

func (s *Stub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
		var data []map[string]string
		for _, m := range s.Models {
			data = append(data, map[string]string{"id": m, "object": "model"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		return
	}
	// The next-prompt call after a turn takes no turn of the script.
	if bytes.Contains(body, []byte("predict the next message")) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"stub-s","object":"chat.completion.chunk","model":"stub-model","choices":[{"index":0,"delta":{"content":"NONE"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
		return
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, Request{At: time.Now(), Body: body})
	var tn turn
	n := s.next
	if s.next < len(s.turns) {
		tn = s.turns[s.next]
	}
	s.next++
	s.mu.Unlock()
	if tn == nil {
		tn = turn{{op: "text", text: "(end of script)"}}
	}

	flusher, _ := w.(http.Flusher)
	started := false
	start := func() {
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			started = true
		}
	}
	send := func(v any) {
		start()
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": "stub-" + strconv.Itoa(n), "object": "chat.completion.chunk", "model": "stub-model",
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	var usage map[string]any
	calls := 0
	for _, st := range tn {
		switch st.op {
		case "text", "reasoning":
			field := "content"
			if st.op == "reasoning" {
				field = "reasoning_content"
			}
			s.mu.Lock()
			d := Delta{Index: len(s.delta), Text: st.text, SentAt: time.Now()}
			s.delta = append(s.delta, d)
			s.mu.Unlock()
			send(chunk(map[string]any{field: st.text}, nil))
		case "tool":
			send(chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": calls, "id": fmt.Sprintf("call_%d_%d", n, calls), "type": "function",
				"function": map[string]any{"name": st.name, "arguments": st.args},
			}}}, nil))
			calls++
		case "delay", "stall":
			select {
			case <-time.After(st.dur):
			case <-r.Context().Done():
				return
			}
		case "error":
			if started {
				return // mid-stream: the connection just ends
			}
			// A retryable status is retried at once, so a test does not wait
			// out the client's backoff.
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(st.code)
			fmt.Fprintf(w, `{"error":{"message":"stub error %d","type":"stub"}}`, st.code)
			return
		case "usage":
			usage = map[string]any{"prompt_tokens": st.in, "completion_tokens": st.out,
				"prompt_tokens_details": map[string]any{"cached_tokens": st.hit}}
		}
	}
	finish := "stop"
	if calls > 0 {
		finish = "tool_calls"
	}
	last := chunk(map[string]any{}, finish)
	if usage != nil {
		last["usage"] = usage
	}
	send(last)
	start()
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
