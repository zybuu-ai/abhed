//go:build unix

package ui

import (
	"os"
	"testing"
	"time"
)

// A terminal that never answers costs startup at most the short wait, and
// an answer that comes is read: theme, synchronized output, and the keys
// typed around it kept for the prompt.
func TestProbeTerminal(t *testing.T) {
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	start := time.Now()
	theme, sync, rest := probeTerminal(r, devnull, probeWait)
	if took := time.Since(start); took > probeWait+30*time.Millisecond {
		t.Fatalf("a silent terminal held startup %v", took)
	}
	if theme != "" || sync != -1 || len(rest) != 0 {
		t.Fatalf("a silent terminal answered: %q %d %q", theme, sync, rest)
	}

	r2, w2, _ := os.Pipe()
	defer r2.Close()
	defer w2.Close()
	_, _ = w2.WriteString("ab\x1b]11;rgb:ffff/ffff/ffff\x07\x1b[?2026;2$y\x1b[?62;22cc")
	theme, sync, rest = probeTerminal(r2, devnull, probeWait)
	if theme != "light" || sync != 1 || string(rest) != "abc" {
		t.Fatalf("got %q %d %q", theme, sync, rest)
	}
}
