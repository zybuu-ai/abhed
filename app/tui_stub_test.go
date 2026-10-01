package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// tuiStub is a scripted OpenAI-compatible model for the terminal tests. What
// it does is chosen by a keyword in the latest user message, and by how many
// tool results have come back since, so a scenario reads like a script:
// "please edit" reads the file, then edits it, then says it is done.
//
// It streams: each chunk is flushed as it is written, and the time the first
// text chunk of the latest reply left is kept, so a test can measure how long
// the terminal took to show it.
type tuiStub struct {
	ws string

	mu         sync.Mutex
	firstText  time.Time
	requests   int
	lastPrompt string
	// hold, when set, pauses a reply before its first text until released.
	hold chan struct{}
	// suggestion answers the next-prompt call, "" as NONE; suggestCalls
	// counts those calls, which requests leaves out.
	suggestion   string
	suggestCalls int
}

type stubStep struct {
	reasoning string
	text      []string // chunks, sent in order
	pace      time.Duration
	tool      string
	args      map[string]any
	status    int // an HTTP error instead of a reply
}

func (s *tuiStub) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (s *tuiStub) path(rel string) string { return filepath.Join(s.ws, rel) }

func words(text string) []string {
	parts := strings.SplitAfter(text, " ")
	return parts
}

func (s *tuiStub) plan(user string, results int, last string) stubStep {
	u := strings.ToLower(user)
	done := stubStep{text: words("Done. The tool said: " + firstN(strings.ReplaceAll(last, "\n", " "), 60))}
	switch {
	case strings.Contains(u, "please edit"):
		switch results {
		case 0:
			return stubStep{tool: "read", args: map[string]any{"path": s.path("hello.txt")}}
		case 1:
			return stubStep{tool: "edit", args: map[string]any{"path": s.path("hello.txt"),
				"old_string": "hello world", "new_string": "hello, abhed world\nsecond line added"}}
		}
		return done
	case strings.Contains(u, "please write"):
		if results == 0 {
			var lines []string
			for i := 1; i <= 30; i++ {
				lines = append(lines, fmt.Sprintf("line %d", i))
			}
			return stubStep{tool: "write", args: map[string]any{"path": s.path("notes/new.md"), "content": strings.Join(lines, "\n") + "\n"}}
		}
		return done
	case strings.Contains(u, "please fail"):
		if results == 0 {
			return stubStep{tool: "bash", args: map[string]any{"command": "ls /definitely/not/here", "description": "list a missing dir"}}
		}
		return done
	case strings.Contains(u, "please big"):
		if results == 0 {
			return stubStep{tool: "bash", args: map[string]any{"command": "seq 1 3000", "description": "print many lines"}}
		}
		return done
	case strings.Contains(u, "please escape"):
		// A path carrying a clipboard write, a title and a screen erase.
		if results == 0 {
			return stubStep{tool: "write", args: map[string]any{"path": s.path("n\x1b]52;c;U1BPT0Y=\x07o\x1b]0;TITLE\x07t\x1b[2Je.md"), "content": "x\n"}}
		}
		return done
	case strings.Contains(u, "please print"):
		// A command whose output writes a clipboard, a title and an erase.
		if results == 0 {
			return stubStep{tool: "bash", args: map[string]any{"command": `printf 'a\033]52;c;U1BPT0Y=\007b\033]0;TITLE\007c\033[2Jd\n'`, "description": "print"}}
		}
		return done
	case strings.Contains(u, "please spoof"):
		// A command whose tail, after a joiner and a carriage return, would
		// draw over its head.
		if results == 0 {
			return stubStep{tool: "bash", args: map[string]any{"command": "touch pwned #\u200d\r│ $ ls -la                                        ", "description": "list"}}
		}
		return done
	case strings.Contains(u, "please rm"):
		if results == 0 {
			return stubStep{tool: "bash", args: map[string]any{"command": "rm -rf build", "description": "remove build dir"}}
		}
		return done
	case strings.Contains(u, "please read"):
		if results == 0 {
			return stubStep{tool: "read", args: map[string]any{"path": s.path("hello.txt")}}
		}
		return done
	case strings.Contains(u, "please long"):
		return stubStep{text: words(stubLong), pace: time.Millisecond}
	case strings.Contains(u, "please code"):
		// A fence split across deltas, with a line that looks like a heading.
		return stubStep{text: []string{"Here:\n\n``", "`go\n# not a heading\nfunc main() {\n", "\tfmt.Println(\"hi\") // ", "done\n}\n``", "`\n\nAfter the code."}, pace: 5 * time.Millisecond}
	case strings.Contains(u, "please slow"):
		var w []string
		for i := 0; i < 60; i++ {
			w = append(w, fmt.Sprintf("word%d ", i))
		}
		return stubStep{text: w, pace: 30 * time.Millisecond}
	case strings.Contains(u, "please think"):
		return stubStep{reasoning: strings.Repeat("Let me consider the question carefully. ", 20), text: words("The answer is 42.")}
	case strings.Contains(u, "please err"):
		return stubStep{status: 500}
	case strings.Contains(u, "please plan"):
		return stubStep{text: words("## Plan\n\n1. Read the file.\n2. Change the greeting.\n3. Run the tests.")}
	}
	return stubStep{text: words("Hello from the stub. You said: " + firstN(strings.ReplaceAll(user, "\n", " / "), 200)), pace: 2 * time.Millisecond}
}

func firstN(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

const stubLong = "# Summary of changes\n\nHere is **what I found** in the `hello.txt` file and a few *notes*:\n\n" +
	"1. The greeting is fine.\n2. The trailing newline is missing.\n   - nested bullet one\n   - nested bullet two\n\n" +
	"| File | Lines | Status |\n|------|------:|--------|\n| hello.txt | 3 | ok |\n| main.go | 120 | needs work |\n\n" +
	"```go\npackage main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello, world\")  // a fairly long comment that should wrap in a narrow terminal window for sure\n}\n```\n\n" +
	"> A blockquote line.\n\nSee [the docs](https://example.com/docs) for more. " +
	"Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. " +
	"Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat."

func (s *tuiStub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, `{"data":[{"id":"stub-1","object":"model"}]}`)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" && strings.Contains(string(req.Messages[0].Content), "predict the next message") {
		s.suggest(w)
		return
	}
	user, last, results := "", "", 0
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		var text string
		if json.Unmarshal(m.Content, &text) != nil {
			text = string(m.Content)
		}
		if i == len(req.Messages)-1 {
			last = text
		}
		if m.Role == "tool" {
			results++
			continue
		}
		if m.Role == "user" && !strings.HasPrefix(text, "<system") {
			user = text
			break
		}
	}
	s.mu.Lock()
	s.requests++
	s.lastPrompt = user
	hold := s.hold
	s.mu.Unlock()

	step := s.plan(user, results, last)
	if step.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(step.status)
		_, _ = io.WriteString(w, `{"error":{"message":"stub: upstream exploded (simulated 500)"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	send := func(delta map[string]any, finish string) {
		ch := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			ch["finish_reason"] = finish
		}
		b, _ := json.Marshal(map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "stub-1", "choices": []any{ch}})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	if step.reasoning != "" {
		send(map[string]any{"role": "assistant", "reasoning_content": step.reasoning}, "")
	}
	if step.tool != "" {
		args, _ := json.Marshal(step.args)
		send(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("call_%d", results+1),
			"type": "function", "function": map[string]any{"name": step.tool, "arguments": string(args)}}}}, "")
		send(map[string]any{}, "tool_calls")
	} else {
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		for i, chunk := range step.text {
			if i > 0 && step.pace > 0 {
				select {
				case <-time.After(step.pace):
				case <-r.Context().Done():
					return
				}
			}
			if i == 0 {
				s.mu.Lock()
				s.firstText = time.Now()
				s.mu.Unlock()
			}
			send(map[string]any{"content": chunk}, "")
		}
		send(map[string]any{}, "stop")
	}
	b, _ := json.Marshal(map[string]any{"id": "x", "object": "chat.completion.chunk", "choices": []any{},
		"usage": map[string]any{"prompt_tokens": 1200, "completion_tokens": 80, "total_tokens": 1280}})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	if fl != nil {
		fl.Flush()
	}
}

func (s *tuiStub) firstTextAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstText
}

func (s *tuiStub) prompt() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPrompt
}

// suggest answers the next-prompt call with the scripted suggestion.
func (s *tuiStub) suggest(w http.ResponseWriter) {
	s.mu.Lock()
	s.suggestCalls++
	text := s.suggestion
	s.mu.Unlock()
	if text == "" {
		text = "NONE"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	b, _ := json.Marshal(map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "stub-1",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": "stop"}}})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
}
