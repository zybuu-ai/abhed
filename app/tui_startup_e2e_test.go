//go:build unix

package app

import (
	"strings"
	"testing"
	"time"
)

// Startup to a ready prompt on a terminal that never answers its queries:
// the first time, and again once the terminal's silence is known.
func TestTUIStartupOnASilentTerminal(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	// Without a container runtime on PATH: probing one is first-run work of
	// its own, measured apart from the terminal's.
	t.Setenv("PATH", "/usr/bin:/bin")
	for i := 0; i < 2; i++ {
		start := time.Now()
		r := startTUI(t, stub, ws, 80, 24)
		r.waitFor("the prompt", false, func(s string) bool { return strings.Contains(s, "shift+tab") })
		took := time.Since(start)
		t.Logf("run %d: prompt ready %v after start", i+1, took)
		// Generous for a loaded CI machine; the point is that a silent
		// terminal costs a few tens of milliseconds, not a timeout.
		if took > 2*time.Second {
			t.Fatalf("startup took %v", took)
		}
		r.exit()
	}
}
