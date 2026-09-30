package ui

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// The redraw budgets: typing at the end of the line at an idle
// prompt is at most 64 bytes a key; a mid-line edit at most 256; typing
// during a turn, with the activity line running, at most 160. Redrawing the
// whole dock on every key cost 833 to 1,386 bytes.
func TestKeystrokeByteBudgets(t *testing.T) {
	measure := func(g *rig, keys string) (mean float64, worst int64) {
		var total int64
		for _, k := range keys {
			g.settle()
			g.out.mark()
			g.keys(string(k))
			g.settle()
			n := g.out.bytes()
			total += n
			worst = max(worst, n)
		}
		return float64(total) / float64(len([]rune(keys))), worst
	}
	const text = "the quick brown fox jumps over the lazy dog again and again"

	g := newRig(t, 100, 30)
	g.settle()
	g.keys("x")
	g.settle()
	mean, worst := measure(g, text)
	t.Logf("idle, end of line: mean %.1f B, worst %d B", mean, worst)
	if worst > 64 {
		t.Errorf("idle end-of-line key cost %d bytes, budget 64", worst)
	}

	g.keys("\x01") // to the start: every key now shifts the rest of the line
	g.settle()
	mean, worst = measure(g, "abcdefghij")
	t.Logf("idle, mid-line: mean %.1f B, worst %d B", mean, worst)
	if worst > 256 {
		t.Errorf("mid-line key cost %d bytes, budget 256", worst)
	}

	g2 := newRig(t, 100, 30)
	g2.lr.Quiet(true)
	g2.lr.d.mu.Lock()
	g2.lr.d.act = activity{on: true, since: time.Now()}
	g2.lr.d.mu.Unlock()
	g2.keys("y")
	g2.settle()
	sizes := measureEach(g2, text)
	p95 := percentile(sizes, 95)
	t.Logf("during a turn: p95 %d B, worst %d B", p95, sizes[len(sizes)-1])
	if p95 > 160 {
		t.Errorf("a key during a turn cost %d bytes at the 95th percentile, budget 160", p95)
	}
}

// The spinner is at most ten frames a second and each frame is small: it
// rewrites the glyph, not the line (at most 120 B a frame).
func TestSpinnerFrameBudget(t *testing.T) {
	g := newRig(t, 100, 30)
	g.lr.Quiet(true)
	g.lr.d.mu.Lock()
	g.lr.d.act = activity{on: true, since: time.Now()}
	g.lr.d.draw()
	g.lr.d.mu.Unlock()
	g.settle()
	g.out.mark()
	time.Sleep(time.Second)
	n := g.out.bytes()
	t.Logf("spinner: %d bytes in one second", n)
	if n > 10*120 {
		t.Errorf("spinner wrote %d bytes in a second, budget 1200", n)
	}
	if !strings.ContainsAny(g.term.Text(), strings.Join(spinFrames, "")) {
		t.Fatalf("no spinner on screen:\n%s", g.term.Dump())
	}
}

// measureEach sends keys one at a time and returns what each cost, sorted.
func measureEach(g *rig, keys string) []int {
	var sizes []int
	for _, k := range keys {
		g.settle()
		g.out.mark()
		g.keys(string(k))
		g.settle()
		sizes = append(sizes, int(g.out.bytes()))
	}
	slices.Sort(sizes)
	return sizes
}

func percentile(sorted []int, p int) int {
	if len(sorted) == 0 {
		return 0
	}
	i := (len(sorted)*p + 99) / 100
	return sorted[min(max(i-1, 0), len(sorted)-1)]
}
