//go:build unix

package ui

import (
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// A terminal's answer to a startup question can arrive straight after the
// first Enter, on a loaded machine or a slow link. It is not more pasted
// text: the Enter still submits, and the answer is not typed.
func TestALateAnswerAfterEnterStillSubmits(t *testing.T) {
	for name, reply := range map[string]string{
		"colour":     "\x1b]11;rgb:0000/0000/0000\x07",
		"attributes": "\x1b[?62;22c",
		"mode":       "\x1b[?2026;2$y",
	} {
		g := newRig(t, 80, 24)
		g.keys("fix it\r" + reply)
		got, err := g.line()
		if err != nil || got != "fix it" {
			t.Errorf("%s: got %q, %v; want the line submitted", name, got, err)
		}
	}
}

// Keys typed before raw mode wait in the cooked terminal, which has made
// their Enter a line feed. They are kept, and the Enter is an Enter again.
func TestKeysTypedBeforeRawModeKeepTheirEnter(t *testing.T) {
	ptm, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	defer func() { _ = ptm.Close(); _ = tty.Close() }()
	if _, err := ptm.WriteString("early task\r"); err != nil {
		t.Fatal(err)
	}
	// The line discipline takes the keys on its own time; wait until it has.
	ready := readyFunc(tty)
	if !ready(2 * time.Second) {
		t.Fatal("the typed line never reached the terminal")
	}
	state, err := term.MakeRaw(int(tty.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = term.Restore(int(tty.Fd()), state) }()
	if got := string(typedBeforeRaw(tty)); got != "early task\r" {
		t.Fatalf("got %q, want the line with its Enter", got)
	}
	if got := typedBeforeRaw(tty); len(got) != 0 {
		t.Fatalf("read again: %q", got)
	}
}
