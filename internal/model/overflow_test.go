package model

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Each provider's refusal of an over-long prompt is recognised, with the
// limit it named; nothing else is.
func TestContextOverflow(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		limit int
	}{
		{"anthropic", fmt.Errorf("anthropic returned %w", &StatusError{Status: 400, Attempts: 1,
			Body: `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 200397 tokens > 200000 maximum"}}`}), 200000},
		{"bedrock", fmt.Errorf("anthropic returned %w", &StatusError{Status: 400, Body: `{"message":"Input is too long for requested model."}`}), 0},
		{"openai", fmt.Errorf("endpoint returned %w", &StatusError{Status: 400, Body: `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130512 tokens. Please reduce the length of the messages.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`}), 128000},
		{"openai-limit", fmt.Errorf("endpoint returned %w", &StatusError{Status: 400, Body: `{"error":{"message":"Input tokens exceed the configured limit of 272000 tokens. Your messages resulted in 280000 tokens.","code":"context_length_exceeded"}}`}), 272000},
		{"vllm", fmt.Errorf("endpoint returned %w", &StatusError{Status: 400, Body: `{"object":"error","message":"This model's maximum context length is 32768 tokens. However, you requested 33010 tokens (33010 in the messages, 0 in the completion).","code":400}`}), 32768},
		{"gemini", fmt.Errorf("gemini returned %w", &StatusError{Status: 400, Body: `{"error":{"code":400,"message":"The input token count (1200000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`}), 1048576},
		{"llamacpp", fmt.Errorf("endpoint returned %w", &StatusError{Status: 400, Body: `{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error","n_prompt_tokens":5000,"n_ctx":4096}}`}), 4096},
		{"ollama", fmt.Errorf("endpoint returned %w", &StatusError{Status: 400, Body: `{"error":{"message":"the input length exceeds the context length of 8192"}}`}), 8192},
		{"mistral", fmt.Errorf("endpoint returned %w", &StatusError{Status: 400, Body: `{"message":"Prompt contains 140000 tokens and 0 draft tokens, too large for model with 131072 maximum context length"}`}), 131072},
		{"groq", fmt.Errorf("endpoint returned %w", &StatusError{Status: 400, Body: `{"error":{"message":"Please reduce the length of the messages or completion.","type":"invalid_request_error","code":"context_length_exceeded"}}`}), 0},
		{"watsonx", errors.New("watsonx returned 400 Bad Request: the number of input tokens 140000 cannot exceed the total tokens limit 131072 for this model"), 131072},
		{"tgi", fmt.Errorf("endpoint returned %w", &StatusError{Status: 422, Body: `{"error":"Input validation error: inputs must have less than 4096 input tokens. Given: 5000","error_type":"validation"}`}), 4096},
	}
	for _, c := range cases {
		ok, limit := ContextOverflow(c.err)
		if !ok || limit != c.limit {
			t.Errorf("%s: overflow %v limit %d, want true %d", c.name, ok, limit, c.limit)
		}
	}
	for _, e := range []error{
		nil,
		errors.New("connection refused"),
		&StatusError{Status: 429, Body: "rate limited: prompt is too long for this tier"},
		&StatusError{Status: 500, Body: "maximum context length is 10"},
		&StatusError{Status: 400, Body: `{"error":{"message":"max_tokens: 200000 > 128000, which is the maximum allowed number of output tokens"}}`},
		&StatusError{Status: 401, Body: "invalid x-api-key"},
		&TimeoutError{},
		// The reply cap, not the prompt, was over: lowering max_tokens is the fix.
		&StatusError{Status: 400, Body: `{"error":{"message":"This model's maximum context length is 128000 tokens. However, you requested 140000 tokens (20000 in the messages, 120000 in the completion). Please reduce the length of the messages or completion.","code":"context_length_exceeded"}}`},
	} {
		if ok, _ := ContextOverflow(e); ok {
			t.Errorf("%v read as an overflow", e)
		}
	}
}

// A prompt cut by a server that truncates rather than refuses shows as a
// read count well short of what was sent, only for such a server.
func TestTruncatedPrompt(t *testing.T) {
	ollama := Profile{SilentTruncation: true}
	if !TruncatedPrompt(ollama, 20_000, 4_096) {
		t.Fatal("a 20k prompt read as 4k is cut")
	}
	if TruncatedPrompt(ollama, 20_000, 18_000) {
		t.Fatal("an estimate's error is not a cut")
	}
	if TruncatedPrompt(ollama, 3_000, 1_000) {
		t.Fatal("a small prompt is never judged")
	}
	if TruncatedPrompt(Profile{}, 20_000, 4_096) {
		t.Fatal("a server that refuses is never judged truncated")
	}
}

// The adapters' own errors, from a real 400 on the wire, are recognised, and
// the limit learned lowers the window: the founder's session, which a 400 at
// 200,397 tokens left unrecoverable.
func TestAdapterRefusalIsRecognised(t *testing.T) {
	bodies := map[string]string{
		"anthropic": `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 200397 tokens > 200000 maximum"}}`,
		"openai":    `{"error":{"message":"This model's maximum context length is 200000 tokens. However, your messages resulted in 200397 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`,
		"gemini":    `{"error":{"code":400,"message":"The input token count (200397) exceeds the maximum number of tokens allowed (200000).","status":"INVALID_ARGUMENT"}}`,
	}
	for typ, body := range bodies {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(body))
		}))
		model := map[string]string{"anthropic": "claude-sonnet-5-5", "openai": "gpt-5", "gemini": "gemini-2.5-pro"}[typ]
		a, err := New(Spec{Type: typ, Model: model, BaseURL: srv.URL, APIKey: "k"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = a.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
		srv.Close()
		ok, limit := ContextOverflow(err)
		if !ok || limit != 200000 {
			t.Errorf("%s: %v read as overflow=%v limit=%d", typ, err, ok, limit)
			continue
		}
		if !LearnWindow(a, limit, true) || a.Profile().ContextWindow != 200000 || a.Profile().WindowSource != WindowLearned {
			t.Errorf("%s: window %+v", typ, a.Profile())
		}
	}
}
