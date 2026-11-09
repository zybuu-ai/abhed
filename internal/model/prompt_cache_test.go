package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Which servers are asked to cache, and how: only those documented to take
// the field, so no other server is sent one it may refuse.
func TestPromptCacheMode(t *testing.T) {
	for _, c := range []struct {
		typ, base, model, extra, want string
	}{
		{"openai", "", "gpt-4o", "", CacheKey},
		{"openai", "https://api.openai.com/v1/", "gpt-4o", "", CacheKey},
		{"openai", "https://proxy.example/v1", "gpt-4o", "", ""}, // a drop-in replacement
		{"openai", "", "gpt-4o", "off", ""},
		{"openrouter", "", "anthropic/claude-sonnet-4.5", "", CacheControl},
		{"openrouter", "", "google/gemini-2.5-pro", "", CacheControl},
		{"openrouter", "", "openai/gpt-4o", "", ""},
		{"deepseek", "", "deepseek-chat", "", ""}, // caches on its own
		{"vllm", "", "m", "", ""},
		{"openai-compatible", "http://litellm:4000/v1", "claude", "", ""},
		{"openai-compatible", "http://litellm:4000/v1", "claude", "cache_control", CacheControl},
		{"groq", "", "llama", "prompt_cache_key", CacheKey},
	} {
		s := Spec{Type: c.typ, BaseURL: c.base, Model: c.model, APIKey: "k", NoAmbientKey: true}
		if c.extra != "" {
			s.Extra = map[string]string{"prompt_cache": c.extra}
		}
		a, err := New(s)
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if got := a.(*OpenAICompatible).PromptCache; got != c.want {
			t.Errorf("%s %q %s extra %q: %q, want %q", c.typ, c.base, c.model, c.extra, got, c.want)
		}
	}
	if _, err := New(Spec{Type: "openai", Model: "m", APIKey: "k", Extra: map[string]string{"prompt_cache": "yes"}}); err == nil {
		t.Fatal("an unknown prompt_cache value must be refused")
	}
}

func cacheRequest(t *testing.T, mode string, req Request) map[string]any {
	t.Helper()
	c := NewOpenAICompatible("http://unused", "", "m", Profile{})
	c.PromptCache = mode
	b, err := json.Marshal(c.buildRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	return body
}

var cacheConversation = Request{System: "You are Abhed.", Messages: []Message{
	{Role: RoleUser, Content: "first"},
	{Role: RoleAssistant, Content: "ok"},
	{Role: RoleUser, Content: "second"},
}}

// prompt_cache_key is stable for a session's prefix, says nothing of its
// text, and is sent only in that mode.
func TestPromptCacheKey(t *testing.T) {
	a := cacheRequest(t, CacheKey, cacheConversation)
	key, _ := a["prompt_cache_key"].(string)
	if !strings.HasPrefix(key, "abhed-") || strings.Contains(key, "Abhed.") {
		t.Fatalf("key %q", key)
	}
	more := cacheConversation
	more.Messages = append(append([]Message(nil), more.Messages...), Message{Role: RoleAssistant, Content: "x"}, Message{Role: RoleUser, Content: "third"})
	if b := cacheRequest(t, CacheKey, more); b["prompt_cache_key"] != key {
		t.Fatal("the key changed as the conversation grew")
	}
	for _, mode := range []string{"", CacheControl} {
		if _, ok := cacheRequest(t, mode, cacheConversation)["prompt_cache_key"]; ok {
			t.Fatalf("mode %q sent prompt_cache_key", mode)
		}
	}
}

// cache_control marks the system prompt and the last user message only, and
// no other mode sends it.
func TestPromptCacheControl(t *testing.T) {
	body := cacheRequest(t, CacheControl, cacheConversation)
	msgs := body["messages"].([]any)
	marked := func(m any) bool {
		parts, ok := m.(map[string]any)["content"].([]any)
		if !ok || len(parts) == 0 {
			return false
		}
		cc, _ := parts[len(parts)-1].(map[string]any)["cache_control"].(map[string]any)
		return cc["type"] == "ephemeral"
	}
	if !marked(msgs[0]) || !marked(msgs[3]) {
		t.Fatalf("system or last message unmarked: %v", msgs)
	}
	if marked(msgs[1]) || marked(msgs[2]) {
		t.Fatalf("an earlier message was marked: %v", msgs)
	}
	if _, isString := msgs[1].(map[string]any)["content"].(string); !isString {
		t.Fatal("an unmarked message must stay a plain string")
	}
	for _, mode := range []string{"", CacheKey} {
		b, _ := json.Marshal(cacheRequest(t, mode, cacheConversation))
		if strings.Contains(string(b), "cache_control") {
			t.Fatalf("mode %q sent cache_control", mode)
		}
	}
}

// Cached tokens are read as each server names them, and recorded.
func TestPromptCacheUsage(t *testing.T) {
	for _, c := range []struct {
		name, usage          string
		cached, write, input int
	}{
		{"openai", `{"prompt_tokens":1000,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":768}}`, 768, 0, 1000},
		{"deepseek", `{"prompt_tokens":1000,"completion_tokens":5,"prompt_cache_hit_tokens":640,"prompt_cache_miss_tokens":360}`, 640, 0, 1000},
		{"anthropic via proxy", `{"prompt_tokens":1000,"completion_tokens":5,"cache_read_input_tokens":500,"cache_creation_input_tokens":200}`, 500, 200, 1000},
	} {
		srv := sseServer(t, `{"choices":[{"delta":{"content":"hi"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":`+c.usage+`}`)
		_, _, _, u, errs := collect(t, NewOpenAICompatible(srv.URL, "", "m", Profile{}))
		srv.Close()
		if len(errs) > 0 || u == nil || u.CachedInputTokens != c.cached || !u.CacheReported || u.CacheWriteTokens != c.write || u.InputTokens != c.input {
			t.Errorf("%s: %+v %v", c.name, u, errs)
		}
	}
}

// End to end: an adapter in cache_control mode sends the marks on the wire.
func TestPromptCacheOnTheWire(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	a, err := New(Spec{Type: "openrouter", BaseURL: srv.URL, Model: "anthropic/claude-sonnet-4.5", APIKey: "k", Extra: map[string]string{"prompt_cache": "cache_control"}})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := a.Complete(context.Background(), cacheConversation)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if !strings.Contains(string(got), `"cache_control":{"type":"ephemeral"}`) {
		t.Fatalf("body: %s", got)
	}
}
