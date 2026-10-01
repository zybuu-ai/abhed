package clitest

import (
	"flag"
	"sort"
	"testing"
	"time"
)

// Budgets are the performance limits the plan sets (p95 unless noted). A
// failing budget fails the test like any other assertion; they change only
// in a pull request the founder sees.
var Budgets = struct {
	BytesPerKeyIdle      int           // typing at the end of the line at an idle prompt
	BytesPerKeyEdit      int           // mid-line edit or wrap
	BytesPerKeyTurn      int           // typing while a turn streams
	StreamOverheadPerSec int           // bytes beyond the text while streaming
	FirstToken           time.Duration // stub delta to screen, first token
	DeltaSteady          time.Duration // stub delta to screen, steady state
	StartupWarm          time.Duration // spawn to prompt ready
	StartupCold          time.Duration // first run in a fresh HOME
	ApprovalDrawn        time.Duration
	PasteBytes           int // a raw 25-line paste
}{
	BytesPerKeyIdle:      64,
	BytesPerKeyEdit:      256,
	BytesPerKeyTurn:      160,
	StreamOverheadPerSec: 2048,
	FirstToken:           50 * time.Millisecond,
	DeltaSteady:          33 * time.Millisecond,
	StartupWarm:          150 * time.Millisecond,
	StartupCold:          400 * time.Millisecond,
	ApprovalDrawn:        50 * time.Millisecond,
	PasteBytes:           20 << 10,
}

// raceSlack scales time budgets under the race detector, which slows the
// binary several times over; byte budgets are not scaled.
func raceSlack(d time.Duration) time.Duration {
	if raceEnabled {
		return d * 8
	}
	return d
}

// TimeBudget is d as a test should assert it: scaled under -race.
func TimeBudget(d time.Duration) time.Duration { return raceSlack(d) }

// P95 is the 95th percentile of xs (nearest rank).
func P95[T int | time.Duration](xs []T) T {
	if len(xs) == 0 {
		return 0
	}
	s := append([]T(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := (95*len(s)+99)/100 - 1
	return s[max(0, min(i, len(s)-1))]
}

// Mean is the arithmetic mean of xs.
func Mean(xs []int) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0
	for _, x := range xs {
		sum += x
	}
	return float64(sum) / float64(len(xs))
}

// KeyBytes types each key in turn and returns the bytes the binary wrote
// for each, waiting for the output to settle (settle of silence, at most
// 500 ms) before the next.
func KeyBytes(h Harness, keys []string, settle time.Duration) []int {
	out := make([]int, 0, len(keys))
	for _, k := range keys {
		h.WaitQuiet(settle, 500*time.Millisecond)
		h.MarkBytes()
		sent := time.Now()
		h.Type(k)
		h.WaitSettled(sent, settle, 500*time.Millisecond)
		out = append(out, h.BytesSinceMark())
	}
	return out
}

// AssertBytesPerKey fails t when the p95 of per-key bytes exceeds budget.
func AssertBytesPerKey(t testing.TB, what string, per []int, budget int) {
	t.Helper()
	if p := P95(per); p > budget {
		t.Errorf("%s: p95 %d bytes per key (mean %.0f), budget %d", what, p, Mean(per), budget)
	}
}

// AssertWithin fails t when got exceeds the budget, scaled under -race.
func AssertWithin(t testing.TB, what string, got, budget time.Duration) {
	t.Helper()
	if got > raceSlack(budget) {
		t.Errorf("%s: %v, budget %v", what, got.Round(time.Millisecond), raceSlack(budget))
	}
}

// PromptGlyph is the prompt the interactive CLI draws when it is ready.
const PromptGlyph = "▲"

// runPending runs the tests that wait on another track's work.
var runPending = flag.Bool("clitest-run-pending", false, "run the clitest tests marked pending on another track's work")

// PendingEnv lists the pending items CI accepts a skip for, such as "A1 A2".
// A skip for any other reason fails the CI step that checks them, so an
// end-to-end test that stops running is noticed; scripts/ci/clitest-skips.py
// is that check.
const PendingEnv = "ABHED_CLITEST_PENDING"

// Pending skips t while the plan item it waits on (such as "A1", Track A's
// editor) is unbuilt, saying so in a form CI checks. With
// -clitest-run-pending it runs instead.
func Pending(t testing.TB, item, why string) {
	t.Helper()
	if *runPending {
		return
	}
	t.Skipf("clitest: pending (%s): %s", item, why)
}
