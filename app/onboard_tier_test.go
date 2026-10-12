package app

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// lockedBuf is a writer read while another goroutine writes it.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// When the banner said "none or stronger (checking)" and the answer is none,
// one line says so; the status line knows the tier is still being checked.
func TestSandboxThatEndsAtNoneIsSaid(t *testing.T) {
	l := &lazySandbox{done: make(chan struct{}), floor: sandbox.TierNone}
	if !l.checking() {
		t.Fatal("not checking before the answer")
	}
	var out lockedBuf
	l.noteIfNone(&out)
	l.sb = sandbox.NewNone(sandbox.Policy{})
	close(l.done)
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && out.String() == ""; time.Sleep(10 * time.Millisecond) {
	}
	if !strings.Contains(out.String(), "sandbox: none") {
		t.Fatalf("no line: %q", out.String())
	}
	if l.checking() {
		t.Fatal("still checking after the answer")
	}
	// Resolved before the banner, nothing more is said: the banner named it.
	var quiet lockedBuf
	l.noteIfNone(&quiet)
	time.Sleep(50 * time.Millisecond)
	if quiet.String() != "" {
		t.Fatalf("said again: %q", quiet.String())
	}
}
