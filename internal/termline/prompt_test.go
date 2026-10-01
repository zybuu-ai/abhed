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

// Where the terminal cannot be asked, only Abhed's own prompt counts as the
// shell back at it; a password prompt or a progress line does not.
func TestPromptUnaskedNeedsAbhedsPrompt(t *testing.T) {
	p := NewPrompt()
	p.OutputUnasked([]byte("\x1b[?2004h\x1b[2m(sandbox: container)\x1b[0m \x1b[36mws\x1b[0m $ "))
	if !p.At() {
		t.Fatal("the first prompt was missed")
	}
	p.OutputUnasked([]byte("ls")) // keys echoed at the prompt keep it
	if !p.At() {
		t.Fatal("the echo of keys at the prompt was taken for leaving it")
	}
	p.Gave()
	for _, out := range []string{"ls\r\n", "BUSY\r\n", "Password: ", "\r\nDownloading 50%", "\r\n> "} {
		if p.OutputUnasked([]byte(out)); p.At() {
			t.Fatalf("%q taken for the prompt", out)
		}
	}
	if p.OutputUnasked([]byte("\r\n(sandbox: container) ws # ")); !p.At() {
		t.Fatal("the prompt's return was missed")
	}
}
