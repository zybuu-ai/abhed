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

// Infill sends the prefix and suffix to the completions endpoint and takes
// the first choice's text and the usage.
func TestInfillUsesTheCompletionsEndpoint(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "wrong", http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"choices":[{"text":"return a + b"}],"usage":{"prompt_tokens":12,"completion_tokens":5}}`)
	}))
	defer srv.Close()
	a := NewOpenAICompatible(srv.URL+"/v1", "k", "qwen2.5-coder", Profile{})
	res, err := a.Infill(context.Background(), InfillRequest{Prefix: "func add(a, b int) int {\n\t", Suffix: "\n}", MaxTokens: 64, Stop: []string{"\n\n"}})
	if err != nil || res.Text != "return a + b" || res.InputTokens != 12 || res.OutputTokens != 5 {
		t.Fatalf("%+v %v", res, err)
	}
	if got["prompt"] != "func add(a, b int) int {\n\t" || got["suffix"] != "\n}" || got["model"] != "qwen2.5-coder" || got["stream"] != false || got["max_tokens"] != float64(64) {
		t.Fatalf("body %v", got)
	}
}

func TestInfillReportsAnEndpointError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such model", http.StatusNotFound)
	}))
	defer srv.Close()
	a := NewOpenAICompatible(srv.URL, "", "m", Profile{})
	if _, err := a.Infill(context.Background(), InfillRequest{Prefix: "x"}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err %v", err)
	}
}

// chatOnly answers every turn with a fixed text, as a chat model would.
type chatOnly struct {
	reply string
	seen  Request
}

func (c *chatOnly) Name() string                     { return "chat" }
func (c *chatOnly) Profile() Profile                 { return Profile{Name: "chat"} }
func (c *chatOnly) CountTokens(Request) (int, error) { return 0, nil }
func (c *chatOnly) Complete(_ context.Context, req Request) (<-chan Chunk, error) {
	c.seen = req
	ch := make(chan Chunk, 3)
	ch <- Chunk{Type: ChunkReasoning, Text: "thinking about it"}
	ch <- Chunk{Type: ChunkText, Text: c.reply}
	ch <- Chunk{Type: ChunkDone, Usage: &Usage{InputTokens: 20, OutputTokens: 4}}
	close(ch)
	return ch, nil
}

// A chat model is asked with the cursor marked; only its text is taken, a
// fence it added is taken off, and its reasoning is never part of it.
func TestInfillByChat(t *testing.T) {
	c := &chatOnly{reply: "```go\nreturn a + b\n```"}
	res, err := InfillByChat(context.Background(), c, InfillRequest{Prefix: "func add() {\n\t", Suffix: "\n}", Language: "go"})
	if err != nil || res.Text != "return a + b" || res.InputTokens != 20 || res.OutputTokens != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(c.seen.Tools) != 0 || !strings.Contains(c.seen.Messages[0].Content, "func add() {\n\t"+cursor+"\n}") {
		t.Fatalf("request %+v", c.seen)
	}
}

// A chat or reasoning model's reply is made into code: reasoning, fences,
// prose and code repeated from around the cursor are taken out.
func TestCleanInfill(t *testing.T) {
	prefix, suffix := "func add(a, b int) int {\n\tret", "\n\tlog.Print(x)\n}\n"
	for _, tc := range []struct{ name, in, want, dropped string }{
		{"plain", "urn a + b", "urn a + b", ""},
		{"fenced", "Here is the code:\n```go\nreturn a + b\n```\nThis adds them.", "urn a + b", ""},
		{"think", "<think>the user wants a sum</think>\nurn a + b", "urn a + b", ""},
		{"unclosed think", "<think>still thinking about it", "", ""},
		{"harmony", "<|channel|>analysis<|message|>Need sum.<|end|><|start|>assistant<|channel|>final<|message|>urn a + b", "urn a + b", ""},
		{"analysis prefix", "analysisWe need the sum.assistantfinalurn a + b", "urn a + b", ""},
		{"prose", "Below is a **complete, idiomatic implementation** of `add` that returns the sum.", "", "prose"},
		{"prose opener", "Sure! You can return the sum of a and b.", "", "prose"},
		{"echoed line", "\tret" + "urn a + b", "urn a + b", ""},
		{"echoed suffix", "urn a + b\n\tlog.Print(x)", "urn a + b", ""},
		{"closing brace kept", "urn a + b\n}", "urn a + b\n}", ""},
	} {
		got, why := CleanInfill(tc.in, prefix, suffix, false)
		if got != tc.want || (tc.dropped == "") != (why == "") || !strings.Contains(why, tc.dropped) {
			t.Errorf("%s: %q -> %q (%q); want %q", tc.name, tc.in, got, why, tc.want)
		}
	}
	// From a fill-in-the-middle endpoint, a comment being completed is kept.
	if got, why := CleanInfill("This returns the sum.", "// add: ", "", true); got != "This returns the sum." || why != "" {
		t.Errorf("fim comment: %q %q", got, why)
	}
	if _, why := CleanInfill("Here is a **complete** implementation.", "x", "", true); why == "" {
		t.Error("fim prose was kept")
	}
}

// Through a chat turn, a reasoning model's reply (reasoning apart, prose
// and a fence in the text) gives just the code, asked at a low temperature.
func TestInfillByChatWithAReasoningModel(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range []string{
			`{"choices":[{"delta":{"reasoning_content":"The user wants the body of add."}}]}`,
			`{"choices":[{"delta":{"content":"Here is the completion:\n` + "```go" + `\nreturn a + b\n` + "```" + `"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":30}}`,
		} {
			_, _ = io.WriteString(w, "data: "+d+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	a := NewOpenAICompatible(srv.URL+"/v1", "", "gpt-oss-120b", Profile{SupportsStream: true, Sampling: SamplingOpenAI()})
	res, err := InfillByChat(context.Background(), a, InfillRequest{Prefix: "func add(a, b int) int {\n\t", Suffix: "\n}", MaxTokens: 128, Stop: []string{"\n\n\n"}})
	if err != nil || res.Text != "return a + b" || res.Dropped != "" {
		t.Fatalf("%+v %v", res, err)
	}
	if got["temperature"] != 0.1 || got["max_tokens"] != float64(512) {
		t.Fatalf("request %v", got)
	}
	sys, _ := got["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "Output only the exact code") {
		t.Fatalf("system %q", sys)
	}
}
