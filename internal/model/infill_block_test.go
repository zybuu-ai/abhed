package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A suggestion is one logical block: the statement at the cursor, and the
// body of a block it opens, ending at a blank line or a line back at its
// indentation; an answer that has not shown its end yet is not complete.
func TestOneBlock(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, text, want string
		complete                 bool
	}{
		{"a statement then a sibling", "def word_count(text):\n    ",
			"\"\"\"Count the words.\"\"\"\n    # Normalise\n    words = text.split()\n", `"""Count the words."""`, true},
		{"a block's body, then a dedent", "def f(",
			"a, b):\n    if a:\n        return b\n    return a\n\ndef g():\n", "a, b):\n    if a:\n        return b\n    return a", true},
		{"a blank line with spaces ends it", "x = 1\n",
			"y = 2\n    \nz = 3\n", "y = 2", true},
		{"a brace block and its closing line", "func add(a, b int) int ",
			"{\n\treturn a + b\n}\n\nfunc sub() {\n", "{\n\treturn a + b\n}", true},
		{"an open bracket goes on", "total = sum(",
			"[\n    a,\n    b,\n])\nprint(total)\n", "[\n    a,\n    b,\n])", true},
		{"a docstring over lines goes on", "def f():\n    ",
			"\"\"\"One.\n\n    Two.\n    \"\"\"\n    return 1\n", "\"\"\"One.\n\n    Two.\n    \"\"\"", true},
		{"a word that starts like a closer is not one", "if x {\n",
			"\tendpoint := 1\n\tother()\n", "\tendpoint := 1", true},
		{"a sibling known before its line ends", "x = 1\n", "y = 2\nz", "y = 2", true},
		{"an answer starting on a new line", "def f():",
			"\n    return 1\n    x = 2\n", "\n    return 1", true},
		{"unfinished first line", "x = ", "foo(", "foo(", false},
		{"unfinished next line", "if x:\n", "    y = 2\n        z", "    y = 2\n        z", false},
		{"ends with the answer", "x = 1\n", "y = 2\n", "y = 2\n", false},
		{"an end marker cuts", "x = ", "1<|endoftext|>junk\n", "1", false},
		{"bounded", "def f():\n", "def g():\n" + strings.Repeat("    pass\n", 20), "def g():\n" + strings.TrimSuffix(strings.Repeat("    pass\n", maxBlockLines-1), "\n"), true},
	} {
		got, complete := OneBlock(tc.prefix, tc.text)
		if got != tc.want || complete != tc.complete {
			t.Errorf("%s: got %q %v; want %q %v", tc.name, got, complete, tc.want, tc.complete)
		}
	}
}

// A streamed infill stops reading once Enough says the block is whole; the
// endpoint sees the request end, so the model stops writing. Against a stub
// that writes a token every 20 ms, a one-line answer comes in well under
// the time the whole answer takes.
func TestInfillStreamStopsAtTheBlock(t *testing.T) {
	tokens := []string{"words", " =", " text", ".split", "()\n", "    return", " len", "(words", ")\n"}
	for range 40 {
		tokens = append(tokens, "    # more\n")
	}
	var gone atomic.Bool
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, tok := range tokens {
			out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"text": tok}}})
			if _, err := fmt.Fprintf(w, "data: %s\n\n", out); err != nil {
				gone.Store(true)
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				gone.Store(true)
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	a := NewOpenAICompatible(srv.URL+"/v1", "", "coder", Profile{})
	prefix := "def word_count(text):\n    "
	started := time.Now()
	res, err := a.Infill(context.Background(), InfillRequest{Prefix: prefix, MaxTokens: 128, Stop: FIMStops,
		Enough: func(text string) bool { _, done := OneBlock(prefix, text); return done }})
	took := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := OneBlock(prefix, res.Text); text != "words = text.split()" {
		t.Fatalf("text %q", res.Text)
	}
	if body["stream"] != true || len(body["stop"].([]any)) != 4 {
		t.Fatalf("body %v", body)
	}
	// Six tokens at 20 ms is ~120 ms; the whole answer would be ~1 s.
	if took > 600*time.Millisecond {
		t.Fatalf("took %v: the stream was read past the block", took)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !gone.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !gone.Load() {
		t.Fatal("the endpoint never saw the request end")
	}
	if res.OutputTokens != 6 {
		t.Fatalf("output tokens %d; want one a chunk read", res.OutputTokens)
	}
	t.Logf("one block in %v against a stub writing a token every 20 ms (whole answer ~%v)", took.Round(time.Millisecond), time.Duration(len(tokens))*20*time.Millisecond)
}

// An endpoint that answers a streamed request with one JSON body is read as before.
func TestInfillStreamTakesAPlainAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"text":"x = 1"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer srv.Close()
	a := NewOpenAICompatible(srv.URL+"/v1", "", "coder", Profile{})
	res, err := a.Infill(context.Background(), InfillRequest{Prefix: "y = ", Enough: func(string) bool { return false }})
	if err != nil || res.Text != "x = 1" || res.OutputTokens != 2 {
		t.Fatalf("%+v %v", res, err)
	}
}
