package linediff

import (
	"strings"
	"testing"
)

// Each hunk is a run of changed lines; taking one side of a hunk changes
// that run and nothing else.
func TestHunksTakeOneSide(t *testing.T) {
	a := SplitLines("a\nb\nc\nd\ne\nf\ng\n")
	b := SplitLines("A\nb\nc\nd\ne\nf\nG\nh\n")
	hunks := Hunks(a, b)
	if len(hunks) != 2 {
		t.Fatalf("hunks: %+v", hunks)
	}
	if got := TakeNew(a, b, hunks[0]); got != "A\nb\nc\nd\ne\nf\ng\n" {
		t.Fatalf("take the first hunk's new side: %q", got)
	}
	if got := TakeOld(a, b, hunks[1]); got != "A\nb\nc\nd\ne\nf\ng\n" {
		t.Fatalf("undo the second hunk: %q", got)
	}
	if h := Hunks(a, a); len(h) != 0 {
		t.Fatalf("no change, hunks %+v", h)
	}
	// A file that gains its last newline differs on that line.
	if h := Hunks(SplitLines("x"), SplitLines("x\n")); len(h) != 1 {
		t.Fatalf("final newline: %+v", h)
	}
	if !strings.HasSuffix(strings.Join(SplitLines("p\nq"), ""), "q") {
		t.Fatal("split lost the last line")
	}
}
