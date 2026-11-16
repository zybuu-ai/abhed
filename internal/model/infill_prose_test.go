package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// eofFixture is a recorded answer of a coder model asked at the end of a
// file with no suffix (testdata/fim/qwen2.5-coder-7b-eof-prose.json).
type eofFixture struct {
	Model    string   `json:"model"`
	Language string   `json:"language"`
	Prefix   string   `json:"prefix"`
	Suffix   string   `json:"suffix"`
	Chunks   []string `json:"chunks"`
	Openings []string `json:"openings"`
}

func loadEOFFixture(t *testing.T) eofFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/fim/qwen2.5-coder-7b-eof-prose.json")
	if err != nil {
		t.Fatal(err)
	}
	var f eofFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// At the end of a file the suffix is empty, and Ollama then applies no
// fill-in-the-middle template: the request always carries one, a newline.
// The recorded answer the model gave without one is prose, and is dropped.
func TestInfillAtTheEndOfAFile(t *testing.T) {
	f := loadEOFFixture(t)
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sent)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range f.Chunks {
			out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"text": c}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", out)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	a := NewOpenAICompatible(srv.URL+"/v1", "", f.Model, Profile{})
	res, err := a.Infill(context.Background(), InfillRequest{Prefix: f.Prefix, Suffix: f.Suffix, Language: f.Language, MaxTokens: 128,
		Stop: FIMStops, Enough: func(text string) bool { _, done := OneBlock(f.Prefix, text); return done }})
	if err != nil {
		t.Fatal(err)
	}
	if sent["suffix"] != "\n" || sent["prompt"] != f.Prefix {
		t.Fatalf("sent suffix %q", sent["suffix"])
	}
	if res.Text != "" || !strings.Contains(res.Dropped, "prose") {
		t.Fatalf("the recorded prose was offered: %q (%q)", res.Text, res.Dropped)
	}
	// Each of the other recorded answers is prose too.
	for _, o := range f.Openings {
		if !ReadsAsProse(o, f.Prefix, f.Language) {
			t.Errorf("not taken as prose: %.60q", o)
		}
	}
	// A suffix the editor gives is sent as it is.
	if _, err := a.Infill(context.Background(), InfillRequest{Prefix: "x = ", Suffix: "\nprint(x)\n", Enough: func(string) bool { return false }}); err != nil || sent["suffix"] != "\nprint(x)\n" {
		t.Fatalf("suffix %q %v", sent["suffix"], err)
	}
}

func TestReadsAsProse(t *testing.T) {
	for _, tc := range []struct {
		text, prefix, lang string
		want               bool
	}{
		{"It looks like you're working on a text library.", "def f(x):\n    ", "python", true},
		{"Here's the completed function:\n    return x", "def f(x):\n    ", "python", true},
		{"The function returns the number of words", "func Count(s string) int {\n\t", "go", true},
		{"Sure, I can help with that.", "x = ", "javascript", true},
		// Code, however it starts.
		{"return len(text.split())", "def word_count(text):\n    ", "python", false},
		{"Println(\"hello\")", "fmt.", "go", false},
		{"ValueError(\"overlap must be smaller than size\")", "raise ", "python", false},
		{"SELECT id FROM users WHERE name = ?", "q = ", "go", false},
		{"Optional[int] = None", "x: ", "python", false},
		{"\n    return total", "def f(a, b):\n    total = a + b", "python", false},
		// Words where words belong: a comment, a docstring, a string, a prose language.
		{"Count the words in text and return how many there are.", "def f(text):\n    # ", "python", false},
		{"Count the words in text and return how many there are.", "def f(text):\n    \"\"\"", "python", false},
		{"Count the words in text and return how many there are.", "// ", "go", false},
		{"Hello there from the app", "msg = \"", "python", false},
		{"This guide shows how the parts fit together.", "# Guide\n\n", "markdown", false},
		{"Just x y z", "f = ", "haskell", false},
	} {
		if got := ReadsAsProse(tc.text, tc.prefix, tc.lang); got != tc.want {
			t.Errorf("ReadsAsProse(%q after %q, %s) = %v", tc.text, tc.prefix, tc.lang, got)
		}
	}
}

// The rest of the cursor's line, written again at the end of a one-line
// suggestion, is not inserted twice.
func TestDropLineRest(t *testing.T) {
	for _, tc := range []struct{ text, suffix, want string }{
		{" = size - overlap", " = size - overlap\n    for start in range(0, len(text), step):\n", ""},
		{"a, b)", ")\n", "a, b"},
		{"len(x))", "))", "len(x"},
		{"return x", "\n}\n", "return x"},
		{"x + 1", " # note\n", "x + 1"},
		{"foo()\nbar()", "()\n", "foo()\nbar()"},
		{" + 1", "   \n", " + 1"},
	} {
		if got := DropLineRest(tc.text, tc.suffix); got != tc.want {
			t.Errorf("DropLineRest(%q, %q) = %q, want %q", tc.text, tc.suffix, got, tc.want)
		}
	}
}
