package model

import (
	"context"
	"encoding/json"
	"testing"
)

func drain(t *testing.T, ch <-chan Chunk) (text string, reasoning string, calls []ToolCall, usage Usage, errs []error) {
	t.Helper()
	for c := range ch {
		switch c.Type {
		case ChunkText:
			text += c.Text
		case ChunkReasoning:
			reasoning += c.Text
		case ChunkToolCall:
			calls = append(calls, *c.ToolCall)
		case ChunkError:
			errs = append(errs, c.Err)
		case ChunkDone:
			if c.Usage != nil {
				usage = *c.Usage
			}
		}
	}
	return
}

// Tool arguments arrive as JSON fragments across several events and are only
// parseable once the block ends. Reassembling them wrongly is the failure that
// makes an adapter look like a model that cannot call tools.
func TestAnthropicStreamReassemblesToolArgs(t *testing.T) {
	srv := sseServer(t,
		`{"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":80}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Reading "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the file."}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"read"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"a.go\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`,
		`{"type":"message_stop"}`,
	)
	defer srv.Close()

	a := NewAnthropic(srv.URL, "k", "m", Profile{})
	ch, err := a.Complete(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	text, _, calls, usage, errs := drain(t, ch)

	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if text != "Reading the file." {
		t.Errorf("text = %q", text)
	}
	if len(calls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(calls))
	}
	if calls[0].Name != "read" || calls[0].ID != "call_1" {
		t.Errorf("call = %+v", calls[0])
	}
	var args struct{ Path string }
	if err := json.Unmarshal(calls[0].Args, &args); err != nil {
		t.Fatalf("reassembled args are not valid JSON: %v (%s)", err, calls[0].Args)
	}
	if args.Path != "a.go" {
		t.Errorf("path = %q, want a.go", args.Path)
	}
	if usage.InputTokens != 100 || usage.CachedInputTokens != 80 || usage.OutputTokens != 42 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestAnthropicStreamSeparatesThinking(t *testing.T) {
	srv := sseServer(t,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"weighing it up"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"the answer"}}`,
		`{"type":"message_stop"}`,
	)
	defer srv.Close()

	a := NewAnthropic(srv.URL, "k", "m", Profile{})
	ch, _ := a.Complete(context.Background(), Request{})
	text, reasoning, _, _, _ := drain(t, ch)

	if text != "the answer" {
		t.Errorf("text = %q — reasoning must not leak into the reply", text)
	}
	if reasoning != "weighing it up" {
		t.Errorf("reasoning = %q", reasoning)
	}
}

func TestGeminiStreamParsesCallsAndThoughts(t *testing.T) {
	srv := sseServer(t,
		`{"candidates":[{"content":{"parts":[{"text":"thinking it over","thought":true}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"Here goes."}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":{"path":"a.go"}}}]}}]}`,
		`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":9}}`,
	)
	defer srv.Close()

	g := NewGemini(srv.URL, "k", "gemini-3-pro", Profile{})
	ch, err := g.Complete(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	text, reasoning, calls, usage, errs := drain(t, ch)

	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if text != "Here goes." {
		t.Errorf("text = %q — a thought part must not be treated as reply text", text)
	}
	if reasoning != "thinking it over" {
		t.Errorf("reasoning = %q", reasoning)
	}
	if len(calls) != 1 || calls[0].Name != "read" {
		t.Fatalf("calls = %+v", calls)
	}
	// Gemini issues no call id, and the loop needs one to pair the result back.
	if calls[0].ID == "" {
		t.Error("a synthesized call id is required to match the tool result")
	}
	if usage.InputTokens != 50 || usage.OutputTokens != 9 {
		t.Errorf("usage = %+v", usage)
	}
}

// A stream that ends without its terminal event must still produce ChunkDone,
// or the loop waits on a channel that has already closed.
func TestStreamsAlwaysTerminate(t *testing.T) {
	srv := sseServer(t,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
	)
	defer srv.Close()

	a := NewAnthropic(srv.URL, "k", "m", Profile{})
	ch, _ := a.Complete(context.Background(), Request{})
	sawDone := false
	for c := range ch {
		if c.Type == ChunkDone {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("a truncated stream must still emit ChunkDone")
	}
}

// A provider that leaves the cached-token figure out has not reported a cold
// cache; one that sends it, zero or not, has reported one (issue #74).
func TestCacheReportedFollowsTheProvider(t *testing.T) {
	for _, tc := range []struct {
		name, frame string
		reported    bool
		anthropic   bool
	}{
		{"anthropic with figure", `{"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":0}}}`, true, true},
		{"anthropic without", `{"type":"message_start","message":{"usage":{"input_tokens":100}}}`, false, true},
		{"gemini with figure", `{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":50,"cachedContentTokenCount":0}}`, true, false},
		{"gemini without", `{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":50}}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames := []string{tc.frame}
			if tc.anthropic {
				frames = append(frames, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`, `{"type":"message_stop"}`)
			}
			srv := sseServer(t, frames...)
			t.Cleanup(srv.Close)
			var a Adapter = NewGemini(srv.URL, "k", "m", Profile{})
			if tc.anthropic {
				a = NewAnthropic(srv.URL, "k", "m", Profile{})
			}
			ch, err := a.Complete(context.Background(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, usage, errs := drain(t, ch)
			if len(errs) != 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if usage.CacheReported != tc.reported {
				t.Errorf("CacheReported = %v, want %v", usage.CacheReported, tc.reported)
			}
		})
	}
}
