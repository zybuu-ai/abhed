package app

import (
	"io"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/ui"
)

// scriptedLines replays ReadLine results, then ends input.
type scriptedLines struct{ results []error }

func (s *scriptedLines) ReadLine() (string, error) {
	if len(s.results) == 0 {
		return "", io.EOF
	}
	err := s.results[0]
	s.results = s.results[1:]
	return "next", err
}

// Ctrl-C reaches the session as an interrupt. In raw mode it is a key, and the
// reader used to drop it, so a running turn never heard it.
func TestReadInputForwardsCtrlC(t *testing.T) {
	src := &scriptedLines{results: []error{ui.ErrCtrlC, nil}}
	lines := make(chan string)
	interrupts := make(chan struct{})
	readErr := make(chan struct{})
	go readInput(src, lines, interrupts, readErr)
	select {
	case <-interrupts:
	case <-lines:
		t.Fatal("got a line before the interrupt")
	case <-time.After(2 * time.Second):
		t.Fatal("Ctrl-C was not forwarded")
	}
	if got := <-lines; got != "next" {
		t.Errorf("line after the interrupt = %q", got)
	}
	<-readErr
}

// The first Ctrl-C of a turn cancels it and keeps the session; a second waits
// briefly for the turn and exits with 130, whether or not it stopped.
func TestInterruptTurn(t *testing.T) {
	cancels := 0
	cancel := func() { cancels++ }
	finished := make(chan turnOutcome, 1)
	if code := interruptTurn(1, cancel, finished, time.Second); code != 0 || cancels != 1 {
		t.Fatalf("first Ctrl-C: code %d, cancels %d; want 0, 1", code, cancels)
	}
	start := time.Now()
	if code := interruptTurn(2, cancel, finished, 50*time.Millisecond); code != 130 {
		t.Fatalf("second Ctrl-C, turn stuck: code %d, want 130", code)
	}
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Errorf("exited after %v without waiting for the turn", waited)
	}
	finished <- turnOutcome{}
	start = time.Now()
	if code := interruptTurn(2, cancel, finished, time.Minute); code != 130 || time.Since(start) > time.Second {
		t.Errorf("second Ctrl-C, turn stopping: code %d after %v", code, time.Since(start))
	}
}
