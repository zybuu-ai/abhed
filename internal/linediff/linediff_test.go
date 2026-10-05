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

// Taking a side reproduces either file exactly, hunk by hunk: hunks one kept
// line apart, a file with no final newline, and CRLF or mixed line ends.
func TestTakeOneSideEdgeCases(t *testing.T) {
	cases := []struct {
		name, a, b string
		hunks      int
	}{
		{"adjacent", "a\nb\nc\n", "A\nb\nC\n", 2},
		{"no final newline", "x\ny", "x\nY", 1},
		{"gains lines after no final newline", "x\ny", "x\ny\nz\n", 1},
		{"loses its final newline", "x\ny\n", "x\ny", 1},
		{"crlf", "a\r\nb\r\nc\r\n", "a\r\nB\r\nc\r\n", 1},
		{"line end only", "a\nb\n", "a\r\nb\n", 1},
		{"first and last", "1\n2\n3\n4\n", "one\n2\n3\nfour\n", 2},
	}
	for _, c := range cases {
		a, b := SplitLines(c.a), SplitLines(c.b)
		hs := Hunks(a, b)
		if len(hs) != c.hunks {
			t.Fatalf("%s: hunks %+v, want %d", c.name, hs, c.hunks)
		}
		// Taking every hunk's new side, one at a time over the old file, ends at the new file;
		// undoing every hunk over the new file ends at the old one. From the last hunk, so each
		// earlier hunk is still where it was.
		cur, back := c.a, c.b
		for i := len(hs) - 1; i >= 0; i-- {
			cur = TakeNew(SplitLines(cur), b, hs[i])
			back = TakeOld(a, SplitLines(back), hs[i])
		}
		if cur != c.b {
			t.Errorf("%s: taking every hunk gave %q, want %q", c.name, cur, c.b)
		}
		if back != c.a {
			t.Errorf("%s: undoing every hunk gave %q, want %q", c.name, back, c.a)
		}
		// One hunk alone changes only its own lines.
		if len(hs) == 2 {
			one := TakeNew(a, b, hs[0])
			if got := Hunks(SplitLines(one), b); len(got) != 1 || got[0].NewStart != hs[1].NewStart {
				t.Errorf("%s: after taking the first hunk, left %+v", c.name, got)
			}
			undo := TakeOld(a, b, hs[0])
			if got := Hunks(a, SplitLines(undo)); len(got) != 1 || got[0].OldStart != hs[1].OldStart {
				t.Errorf("%s: after undoing the first hunk, left %+v", c.name, got)
			}
		}
	}
}

// Taking one side of a hunk keeps the rest exactly, for the shapes a review
// meets: hunks one kept line apart, a last line with no newline, and CRLF
// line ends.
func TestTakeOneSideAtTheEdges(t *testing.T) {
	for _, c := range []struct {
		name, a, b string
		hunks      int
		takeNew    []string // TakeNew of each hunk alone
		takeOld    []string // TakeOld of each hunk alone
	}{
		{name: "adjacent hunks", a: "a\nk\nb\n", b: "A\nk\nB\n", hunks: 2,
			takeNew: []string{"A\nk\nb\n", "a\nk\nB\n"}, takeOld: []string{"a\nk\nB\n", "A\nk\nb\n"}},
		{name: "no final newline", a: "a\nb", b: "a\nB", hunks: 1,
			takeNew: []string{"a\nB"}, takeOld: []string{"a\nb"}},
		{name: "final newline gained", a: "a\nb", b: "a\nb\n", hunks: 1,
			takeNew: []string{"a\nb\n"}, takeOld: []string{"a\nb"}},
		{name: "CRLF", a: "a\r\nb\r\nc\r\n", b: "a\r\nB\r\nc\r\n", hunks: 1,
			takeNew: []string{"a\r\nB\r\nc\r\n"}, takeOld: []string{"a\r\nb\r\nc\r\n"}},
		{name: "CRLF to LF on one line", a: "a\r\nb\r\n", b: "a\r\nb\n", hunks: 1,
			takeNew: []string{"a\r\nb\n"}, takeOld: []string{"a\r\nb\r\n"}},
	} {
		a, b := SplitLines(c.a), SplitLines(c.b)
		hs := Hunks(a, b)
		if len(hs) != c.hunks {
			t.Errorf("%s: hunks %+v", c.name, hs)
			continue
		}
		for i, h := range hs {
			if got := TakeNew(a, b, h); got != c.takeNew[i] {
				t.Errorf("%s: TakeNew hunk %d = %q, want %q", c.name, i, got, c.takeNew[i])
			}
			if got := TakeOld(a, b, h); got != c.takeOld[i] {
				t.Errorf("%s: TakeOld hunk %d = %q, want %q", c.name, i, got, c.takeOld[i])
			}
		}
	}
}
