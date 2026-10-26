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
