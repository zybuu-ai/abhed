package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// recordingServer answers every chat request with one short reply and keeps
// each request's body.
func recordingServer(t *testing.T) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"391"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), bodies...)
	}
}

// Thinking off reaches Ollama's /v1 as "reasoning_effort": "none", which it
// reads, as well as "think": false, which it ignores; other servers get only
// "think" unless extra.think_off names the effort field.
func TestThinkingOffSendsReasoningEffortNone(t *testing.T) {
	off := false
	for _, c := range []struct {
		name, typ, path, extra string
		want                   any
	}{
		{"ollama", "ollama", "/v1", "", "none"},
		{"openai-compatible elsewhere", "openai-compatible", "", "", nil},
		{"vllm", "vllm", "/v1", "", nil},
		{"vllm told to", "vllm", "/v1", "reasoning_effort", "none"},
		{"ollama told not to", "ollama", "/v1", "think", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, bodies := recordingServer(t)
			s := Spec{Type: c.typ, BaseURL: srv.URL + c.path, Model: "gemma4:26b", NoAmbientKey: true}
			if c.extra != "" {
				s.Extra = map[string]string{"think_off": c.extra}
			}
			a, err := New(s)
			if err != nil {
				t.Fatal(err)
			}
			ch, err := a.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "17*23?"}}, Params: Params{Think: &off}})
			if err != nil {
				t.Fatal(err)
			}
			for range ch {
			}
			got := bodies()
			if len(got) != 1 || got[0]["think"] != false || got[0]["reasoning_effort"] != c.want {
				t.Fatalf("sent think %v reasoning_effort %v", got[0]["think"], got[0]["reasoning_effort"])
			}
		})
	}
	// An openai-compatible entry on Ollama's loopback port is Ollama, judged
	// by address only (nothing is contacted); another host or type is not.
	for base, typ := range map[string]string{"http://127.0.0.1:11434/v1": "openai-compatible", "http://localhost:11434/v1": "openai-compatible"} {
		if on, err := effortOff(Spec{Type: typ}, base); err != nil || !on {
			t.Fatalf("%s %s: %v %v", typ, base, on, err)
		}
	}
	for _, c := range []struct{ typ, base string }{
		{"openai-compatible", "https://gateway.example.com:11434/v1"},
		{"vllm", "http://gpu:11434/v1"},
		{"vllm", "http://127.0.0.1:11434/v1"},
		{"openai-compatible", "http://127.0.0.1:11435/v1"},
	} {
		if on, _ := effortOff(Spec{Type: c.typ}, c.base); on {
			t.Fatalf("%s %s sends reasoning_effort none", c.typ, c.base)
		}
	}
	if _, err := New(Spec{Type: "ollama", Model: "m", Extra: map[string]string{"think_off": "maybe"}}); err == nil {
		t.Fatal("an unknown think_off must be refused")
	}
}

// With thinking on, nothing new is sent to Ollama.
func TestThinkingOnSendsNoEffortToOllama(t *testing.T) {
	srv, bodies := recordingServer(t)
	a, err := New(Spec{Type: "ollama", BaseURL: srv.URL + "/v1", Model: "gemma4:26b", NoAmbientKey: true})
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := a.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	for range ch {
	}
	if got := bodies(); got[0]["reasoning_effort"] != nil {
		t.Fatalf("sent %v", got[0]["reasoning_effort"])
	}
}

// Only a local server known to stream reasoning is marked so; the official
// API's reasoning models are marked as reasoning out of sight.
func TestReasoningVisibilityProfiles(t *testing.T) {
	for _, c := range []struct {
		typ, base, model string
		streams, hidden  bool
	}{
		{"ollama", "", "gemma4:26b", true, false},
		{"llamacpp", "", "m", true, false},
		{"vllm", "http://127.0.0.1:8000/v1", "m", true, false},
		{"vllm", "https://gpu.example.com/v1", "m", false, false},
		{"openai-compatible", "http://127.0.0.1:4000/v1", "m", false, false},
		{"together", "", "m", false, false},
		{"openai", "", "gpt-5-mini", false, true},
		{"openai", "", "o3", false, true},
		{"openai", "", "gpt-4o", false, false},
	} {
		a, err := New(Spec{Type: c.typ, BaseURL: c.base, Model: c.model, APIKey: "k", NoAmbientKey: true})
		if err != nil {
			t.Fatal(err)
		}
		if p := a.Profile(); p.StreamsReasoning != c.streams || p.ReasoningTokens != c.hidden {
			t.Errorf("%s %s %s: streams %v hidden %v", c.typ, c.base, c.model, p.StreamsReasoning, p.ReasoningTokens)
		}
	}
}
