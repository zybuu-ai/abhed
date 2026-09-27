package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// heldStream sends frames, then holds the response open until the client
// leaves, as a model still generating does.
func heldStream(t *testing.T, frames ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// stopAfterText cancels a call once its first text arrives and returns the
// usage the stream ended with.
func stopAfterText(t *testing.T, a Adapter) Usage {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := a.Complete(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}
	var usage Usage
	for c := range ch {
		switch c.Type {
		case ChunkText:
			cancel()
		case ChunkDone:
			if c.Usage != nil {
				usage = *c.Usage
			}
		}
	}
	return usage
}

// A call stopped part way still counts the tokens its provider had reported:
// Anthropic reports the prompt at the start of the stream.
func TestStoppedAnthropicCallKeepsItsUsage(t *testing.T) {
	srv := heldStream(t,
		`{"type":"message_start","message":{"usage":{"input_tokens":1234,"cache_read_input_tokens":1000}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Working"}}`,
	)
	if u := stopAfterText(t, NewAnthropic(srv.URL, "k", "m", Profile{})); u.InputTokens != 1234 || u.CachedInputTokens != 1000 {
		t.Fatalf("usage after a stop = %+v, want the 1234 input tokens reported", u)
	}
}

// An OpenAI-compatible server that reports usage on every chunk has it kept
// on a stop; one that reports it only at the end has nothing to keep.
func TestStoppedOpenAICallKeepsReportedUsage(t *testing.T) {
	srv := heldStream(t,
		`{"choices":[{"delta":{"content":"Working"}}],"usage":{"prompt_tokens":900,"completion_tokens":3}}`,
	)
	a := NewOpenAICompatible(srv.URL, "k", "m", Profile{})
	if u := stopAfterText(t, a); u.InputTokens != 900 || u.OutputTokens != 3 {
		t.Fatalf("usage after a stop = %+v, want 900 in and 3 out", u)
	}
	quiet := heldStream(t, `{"choices":[{"delta":{"content":"Working"}}]}`)
	if u := stopAfterText(t, NewOpenAICompatible(quiet.URL, "k", "m", Profile{})); u != (Usage{}) {
		t.Fatalf("usage invented for a stream that reported none: %+v", u)
	}
}

// flood repeats frame n times after the first frames, more than an adapter's
// channel holds, so a stop lands while frames are still waiting to be read.
func flood(first []string, frame string, n int) []string {
	out := append([]string(nil), first...)
	for i := 0; i < n; i++ {
		out = append(out, frame)
	}
	return out
}

// A stop noticed between frames, before the connection is seen to close,
// keeps the reported usage too, for each adapter that checks between frames.
func TestStopBetweenFramesKeepsUsage(t *testing.T) {
	cases := []struct {
		name string
		a    func(url string) Adapter
		head []string
		body string
		want int
	}{
		{"anthropic", func(u string) Adapter { return NewAnthropic(u, "k", "m", Profile{}) },
			[]string{`{"type":"message_start","message":{"usage":{"input_tokens":1234}}}`, `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`},
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"w "}}`, 1234},
		{"openai", func(u string) Adapter { return NewOpenAICompatible(u, "k", "m", Profile{}) },
			[]string{`{"choices":[],"usage":{"prompt_tokens":900,"completion_tokens":1}}`},
			`{"choices":[{"delta":{"content":"w "}}]}`, 900},
		{"gemini", func(u string) Adapter { return NewGemini(u, "k", "m", Profile{}) },
			[]string{`{"candidates":[],"usageMetadata":{"promptTokenCount":700,"candidatesTokenCount":1}}`},
			`{"candidates":[{"content":{"parts":[{"text":"w "}]}}]}`, 700},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := heldStream(t, flood(c.head, c.body, 400)...)
			if u := stopAfterText(t, c.a(srv.URL)); u.InputTokens != c.want {
				t.Fatalf("usage after a stop = %+v, want %d in", u, c.want)
			}
		})
	}
}
