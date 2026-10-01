//go:build unix

package clitest

import (
	"strings"
	"testing"
	"time"
)

// Typing at the end of the line at an idle prompt.
func TestBudgetKeysIdle(t *testing.T) {
	h := StartRun(t, Opts{})
	h.WaitOutput(PromptGlyph)
	keys := strings.Split("the quick brown fox jumps over the lazy dog", "")
	per := KeyBytes(h, keys, 30*time.Millisecond)
	t.Logf("bytes per key at an idle prompt: mean %.1f p95 %d", Mean(per), P95(per))
	h.Exit(0)
	Pending(t, "A1", "the line editor redraws the whole line on each key")
	AssertBytesPerKey(t, "idle prompt", per, Budgets.BytesPerKeyIdle)
}

// Stub delta to the screen: each fragment is timed from the moment the stub
// flushed it to the read that brought its text.
func TestBudgetDeltaLatency(t *testing.T) {
	h := StartRun(t, Opts{Script: "delay 100ms\ntext \"Alpha streams first\"\ndelay 100ms\ntext \" then Bravo follows\"\ndelay 100ms\ntext \" and Charlie ends.\\n\""})
	h.WaitOutput(PromptGlyph)
	h.Type("go")
	h.Key(Enter)
	h.WaitOutput("Charlie ends.")
	var lat []time.Duration
	for _, d := range h.Deltas() {
		l, ok := h.Latency(d)
		if !ok {
			t.Fatalf("delta %q never reached the screen", d.Text)
		}
		lat = append(lat, l)
	}
	t.Logf("delta latency: %v", lat)
	h.Exit(0)
	Pending(t, "A2", "the line-buffered renderer holds a delta until its line ends")
	AssertWithin(t, "first token", lat[0], Budgets.FirstToken)
	AssertWithin(t, "steady delta", P95(lat[1:]), Budgets.DeltaSteady)
}
