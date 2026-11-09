package ui

import (
	"bytes"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a buffer safe to write from the dock and read from the test.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// A log line written while the dock holds the screen goes through it, not
// under it: the log package keeps the stderr it had at start, so a warning
// from slog's default once tore the frame.
func TestCaptureTakesLogOutput(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	var screen syncBuf
	l := NewLineReaderOn(r, &screen, 80, 20)
	defer close(l.done)
	before := log.Writer()
	under, uw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer under.Close()
	log.SetOutput(uw) // the terminal under the dock
	t.Cleanup(func() { log.SetOutput(before) })
	restore := l.Capture()
	log.Print("WARN a warning from somewhere")
	time.Sleep(200 * time.Millisecond)
	restore()
	if log.Writer() != uw {
		t.Error("the log's output was not given back")
	}
	_ = uw.Close()
	leaked, _ := io.ReadAll(under)
	if strings.Contains(string(leaked), "a warning from somewhere") {
		t.Fatalf("the log line went under the dock: %q", leaked)
	}
	if !strings.Contains(screen.String(), "a warning from somewhere") {
		t.Fatalf("the log line did not reach the dock: %q", screen.String())
	}
}
