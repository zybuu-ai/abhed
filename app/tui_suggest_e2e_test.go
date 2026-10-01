//go:build unix

package app

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func (s *tuiStub) setSuggestion(text string) {
	s.mu.Lock()
	s.suggestion = text
	s.mu.Unlock()
}

func (s *tuiStub) counts() (requests, suggestCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests, s.suggestCalls
}

// cellOf is where text starts on the screen, or false.
func (r *tuiRun) cellOf(text string) (x, y int, ok bool) {
	for y, line := range r.term.Lines() {
		if i := strings.Index(line, text); i >= 0 {
			return len([]rune(line[:i])), y, true
		}
	}
	return 0, 0, false
}

// After a turn the input shows the offered next prompt dimmed. Enter on the
// empty input sends nothing; Tab fills the input and sends nothing; Enter
// then sends what was filled.
func TestTUIOffersNextPromptDimmed(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	stub.setSuggestion("Run the tests next")
	r := startTUI(t, stub, ws, 100, 30)
	r.send("hi\r")
	r.waitText("Hello from the stub")
	r.waitScreen("Run the tests next")
	x, y, _ := r.cellOf("Run the tests next")
	if c := r.term.CellAt(x, y); !c.Attr.Dim {
		t.Fatalf("the suggestion is not dimmed: %+v\n%s", c, r.term.Dump())
	}
	sent, calls := stub.counts()
	if calls != 1 {
		t.Fatalf("%d suggestion calls, want 1", calls)
	}

	r.send("\r")
	r.quiet(300 * time.Millisecond)
	if now, _ := stub.counts(); now != sent {
		t.Fatalf("Enter on the empty input sent the suggestion (%d requests, was %d)", now, sent)
	}

	r.send("\t")
	r.waitFor("the suggestion filled in", false, func(string) bool {
		x, y, ok := r.cellOf("Run the tests next")
		return ok && !r.term.CellAt(x, y).Attr.Dim
	})
	r.quiet(300 * time.Millisecond)
	if now, _ := stub.counts(); now != sent {
		t.Fatalf("Tab sent the suggestion (%d requests, was %d)", now, sent)
	}

	r.send("\r")
	r.waitText("You said: Run the tests next")
	if got := stub.prompt(); got != "Run the tests next" {
		t.Fatalf("the model got %q", got)
	}
}

// Right at the end of the empty input takes the suggestion too; a typed
// character dismisses it instead.
func TestTUINextPromptRightAcceptsAndTypingDismisses(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	stub.setSuggestion("Explain the change")
	r := startTUI(t, stub, ws, 100, 30)
	r.send("hi\r")
	r.waitScreen("Explain the change")
	r.send("\x1b[C")
	r.waitFor("the suggestion filled in by Right", false, func(string) bool {
		x, y, ok := r.cellOf("Explain the change")
		return ok && !r.term.CellAt(x, y).Attr.Dim
	})
	r.send("\x15") // Ctrl-U clears the line
	r.send("again\r")
	r.waitText("You said: again")
	r.waitScreen("Explain the change")
	r.send("z")
	r.waitFor("the suggestion dismissed", false, func(text string) bool { return !strings.Contains(text, "Explain the change") })
	r.send("\x7f") // back to empty: it stays dismissed
	r.quiet(200 * time.Millisecond)
	if strings.Contains(r.term.Text(), "Explain the change") {
		t.Fatalf("a dismissed suggestion came back:\n%s", r.term.Dump())
	}
}

// suggest.enabled false asks for none.
func TestTUINoSuggestionWhenDisabled(t *testing.T) {
	stub, ws := tuiWorkspace(t, `,"suggest":{"enabled":false}`)
	stub.setSuggestion("Never shown")
	r := startTUI(t, stub, ws, 100, 30)
	r.send("hi\r")
	r.waitText("Hello from the stub")
	r.quiet(300 * time.Millisecond)
	if _, calls := stub.counts(); calls != 0 || strings.Contains(r.term.All(), "Never shown") {
		t.Fatalf("%d suggestion calls with suggest.enabled false", calls)
	}
}

// Line mode, with no input box, asks for none.
func TestLineModeAsksForNoSuggestion(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	stub.setSuggestion("Never shown")
	cmd := mainHelper([]string{"-C", ws})
	cmd.Stdin = strings.NewReader("hi\n")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Hello from the stub") {
		t.Fatalf("no reply:\n%s", out.String())
	}
	if _, calls := stub.counts(); calls != 0 || strings.Contains(out.String(), "Never shown") {
		t.Fatalf("%d suggestion calls in line mode", calls)
	}
}
