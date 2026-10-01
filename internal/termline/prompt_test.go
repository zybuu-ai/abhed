package termline

import "testing"

// The prompt is back only when the shell is in front, out of canonical mode,
// with text on a line begun after the given line's newline.
func TestPromptFollowsTheShell(t *testing.T) {
	p := NewPrompt()
	p.Output([]byte("$ "), true, true)
	if p.At() {
		t.Fatal("canonical mode taken for the prompt")
	}
	p.Output([]byte(""), false, false)
	if p.At() {
		t.Fatal("another program in front taken for the prompt")
	}
	p.Output([]byte(""), true, false)
	if !p.At() {
		t.Fatal("the first prompt was missed")
	}
	p.Gave()
	for _, out := range []string{"ls", "\r\n"} {
		if p.Output([]byte(out), true, false); p.At() {
			t.Fatalf("%q after the line taken for the prompt", out)
		}
	}
	if p.Output([]byte("a b\r\n$ "), true, false); !p.At() {
		t.Fatal("the prompt's return was missed")
	}
}
