package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseServer replays a scripted SSE stream, letting us test the adapter's
// parsing without a live model.
func sseServer(t *testing.T, frames ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func collect(t *testing.T, c *OpenAICompatible) (text, reasoning string, calls []ToolCall, usage *Usage, errs []error) {
	t.Helper()
	ch, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var tb, rb strings.Builder
	for chunk := range ch {
		switch chunk.Type {
		case ChunkText:
			tb.WriteString(chunk.Text)
		case ChunkReasoning:
			rb.WriteString(chunk.Text)
		case ChunkToolCall:
			calls = append(calls, *chunk.ToolCall)
		case ChunkDone:
			usage = chunk.Usage
		case ChunkError:
			errs = append(errs, chunk.Err)
		}
	}
	return tb.String(), rb.String(), calls, usage, errs
}

func TestStreamsText(t *testing.T) {
	srv := sseServer(t,
		`{"choices":[{"delta":{"content":"Hello "}}]}`,
		`{"choices":[{"delta":{"content":"world"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
	)
	defer srv.Close()
	c := NewOpenAICompatible(srv.URL, "", "test", Profile{})
	text, _, _, usage, errs := collect(t, c)
	if len(errs) > 0 {
		t.Fatal(errs[0])
	}
	if text != "Hello world" {
		t.Fatalf("got %q", text)
	}
	if usage == nil || usage.InputTokens != 10 {
		t.Fatalf("usage not captured: %+v", usage)
	}
}

// Tool calls arrive fragmented; the adapter must reassemble them.
func TestReassemblesFragmentedToolCall(t *testing.T) {
	srv := sseServer(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pa"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"/a.go\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	defer srv.Close()
	c := NewOpenAICompatible(srv.URL, "", "test", Profile{})
	_, _, calls, _, errs := collect(t, c)
	if len(errs) > 0 {
		t.Fatal(errs[0])
	}
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	if calls[0].Name != "read" || calls[0].ID != "c1" {
		t.Fatalf("bad call: %+v", calls[0])
	}
	var args map[string]string
	if err := json.Unmarshal(calls[0].Args, &args); err != nil {
		t.Fatalf("args not valid JSON: %s", calls[0].Args)
	}
	if args["path"] != "/a.go" {
		t.Fatalf("args wrong: %v", args)
	}
}

func TestParallelToolCalls(t *testing.T) {
	srv := sseServer(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"read","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"grep","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	defer srv.Close()
	c := NewOpenAICompatible(srv.URL, "", "test", Profile{})
	_, _, calls, _, _ := collect(t, c)
	if len(calls) != 2 || calls[0].Name != "read" || calls[1].Name != "grep" {
		t.Fatalf("expected both calls in order, got %+v", calls)
	}
}

// Reasoning must never leak into content - it would reach tool parsing.
func TestInBandReasoningStripped(t *testing.T) {
	srv := sseServer(t,
		`{"choices":[{"delta":{"content":"<think>let me consider"}}]}`,
		`{"choices":[{"delta":{"content":" the options</think>The answer"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	defer srv.Close()
	c := NewOpenAICompatible(srv.URL, "", "test", Profile{})
	text, reasoning, _, _, _ := collect(t, c)
	if strings.Contains(text, "think") || strings.Contains(text, "consider") {
		t.Fatalf("reasoning leaked into content: %q", text)
	}
	if text != "The answer" {
		t.Fatalf("content wrong: %q", text)
	}
	if !strings.Contains(reasoning, "let me consider the options") {
		t.Fatalf("reasoning not captured: %q", reasoning)
	}
}

func TestOutOfBandReasoningField(t *testing.T) {
	srv := sseServer(t,
		`{"choices":[{"delta":{"reasoning_content":"thinking..."}}]}`,
		`{"choices":[{"delta":{"content":"done"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	defer srv.Close()
	c := NewOpenAICompatible(srv.URL, "", "test", Profile{})
	text, reasoning, _, _, _ := collect(t, c)
	if text != "done" || reasoning != "thinking..." {
		t.Fatalf("text=%q reasoning=%q", text, reasoning)
	}
}

// Malformed tool arguments must surface as an error, not reach a tool.
func TestInvalidToolArgsSurfaced(t *testing.T) {
	srv := sseServer(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"x","function":{"name":"read","arguments":"{not json"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	defer srv.Close()
	c := NewOpenAICompatible(srv.URL, "", "test", Profile{})
	_, _, calls, _, errs := collect(t, c)
	if len(calls) != 0 {
		t.Fatal("malformed call must not be emitted")
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "invalid JSON") {
		t.Fatalf("expected invalid-JSON error, got %v", errs)
	}
}

func TestCachedTokensCaptured(t *testing.T) {
	srv := sseServer(t,
		`{"choices":[{"delta":{"content":"x"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":900}}}`,
	)
	defer srv.Close()
	c := NewOpenAICompatible(srv.URL, "", "test", Profile{})
	_, _, _, usage, _ := collect(t, c)
	if usage.CachedInputTokens != 900 {
		t.Fatalf("cache metric missing: %+v", usage)
	}
}

// An endpoint that sends no cached-token figure must not read as a cold cache;
// one that sends zero has reported a miss.
func TestCacheReportedOnlyWhenSent(t *testing.T) {
	for _, tc := range []struct {
		usage    string
		reported bool
	}{
		{`{"prompt_tokens":1000,"completion_tokens":5}`, false},
		{`{"prompt_tokens":1000,"completion_tokens":5,"prompt_tokens_details":{}}`, false},
		{`{"prompt_tokens":1000,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":0}}`, true},
	} {
		srv := sseServer(t, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":`+tc.usage+`}`)
		c := NewOpenAICompatible(srv.URL, "", "test", Profile{})
		_, _, _, usage, _ := collect(t, c)
		srv.Close()
		if usage.CacheReported != tc.reported || usage.CachedInputTokens != 0 {
			t.Errorf("usage %s: got reported=%v cached=%d", tc.usage, usage.CacheReported, usage.CachedInputTokens)
		}
	}
}

func TestHTTPErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"bad key"}`)
	}))
	defer srv.Close()
	c := NewOpenAICompatible(srv.URL, "", "test", Profile{})
	_, err := c.Complete(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 surfaced, got %v", err)
	}
}

func TestSystemPromptIsFirstMessage(t *testing.T) {
	c := NewOpenAICompatible("http://x", "", "test", Profile{})
	wr := c.buildRequest(Request{
		System:   "SYSTEM",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if len(wr.Messages) != 2 || wr.Messages[0].Role != "system" {
		t.Fatalf("system must lead for prefix caching: %+v", wr.Messages)
	}
}

func TestToolResultCarriesCallID(t *testing.T) {
	c := NewOpenAICompatible("http://x", "", "test", Profile{})
	wr := c.buildRequest(Request{Messages: []Message{
		{Role: RoleTool, Content: "result", ToolCallID: "call_7"},
	}})
	if wr.Messages[0].ToolCallID != "call_7" {
		t.Fatalf("tool_call_id lost: %+v", wr.Messages[0])
	}
}
