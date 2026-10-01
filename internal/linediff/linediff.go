// Package linediff compares two texts line by line: the shortest edit
// script between them, and the hunks it changes.
package linediff

import "strings"

// MaxEditDistance bounds the search. Its memory grows with the square of
// the distance, and a file rewritten that thoroughly reads no better as a
// minimal diff than as old text followed by new.
const MaxEditDistance = 1000

// SplitLines keeps each line's terminator, so a file that gains or loses its
// final newline differs on that line.
func SplitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// EditScript returns one op per line of the diff: ' ' kept, '-' removed from
// a, '+' added from b. The shortest script, by Myers' O(ND) algorithm.
func EditScript(a, b []string) []byte {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ops := make([]byte, 0, len(a)+len(b))
	ops = append(ops, strings.Repeat(" ", pre)...)
	ops = append(ops, shortestEdit(a[pre:len(a)-suf], b[pre:len(b)-suf])...)
	return append(ops, strings.Repeat(" ", suf)...)
}

func shortestEdit(a, b []string) []byte {
	n, m := len(a), len(b)
	replaceAll := []byte(strings.Repeat("-", n) + strings.Repeat("+", m))
	if n == 0 || m == 0 {
		return replaceAll
	}
	limit := min(n+m, MaxEditDistance)
	// v[off+k] is the furthest x reached on diagonal k = x - y.
	off := limit + 1
	v := make([]int, 2*limit+3)
	// trace[d] is v as round d began, cut to the diagonals that round reads.
	var trace [][]int
	found := -1
search:
	for d := 0; d <= limit; d++ {
		trace = append(trace, append([]int(nil), v[off-d-1:off+d+2]...))
		for k := -d; k <= d; k += 2 {
			x := v[off+k-1] + 1
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[off+k] = x
			if x >= n && y >= m {
				found = d
				break search
			}
		}
	}
	if found < 0 {
		return replaceAll
	}

	// Walk back from the end, one round at a time. Built in reverse.
	ops := make([]byte, 0, n+m)
	x, y := n, m
	for d := found; d > 0; d-- {
		prev := trace[d]
		at := func(k int) int { return prev[k+d+1] }
		k := x - y
		prevK := k - 1
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			prevK = k + 1
		}
		prevX := at(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			ops = append(ops, ' ')
			x, y = x-1, y-1
		}
		if x == prevX {
			ops = append(ops, '+')
			y--
		} else {
			ops = append(ops, '-')
			x--
		}
	}
	for ; x > 0 && y > 0; x, y = x-1, y-1 {
		ops = append(ops, ' ')
	}
	for i, j := 0, len(ops)-1; i < j; i, j = i+1, j-1 {
		ops[i], ops[j] = ops[j], ops[i]
	}
	return ops
}

// Hunk is one run of changed lines: OldCount lines of a from OldStart become
// NewCount lines of b from NewStart, both counted from zero.
type Hunk struct {
	OldStart, OldCount int
	NewStart, NewCount int
}

// Hunks are the runs of changed lines between a and b, in order; lines that
// are the same on both sides separate them.
func Hunks(a, b []string) []Hunk {
	var out []Hunk
	ai, bi := 0, 0
	ops := EditScript(a, b)
	for i := 0; i < len(ops); {
		if ops[i] == ' ' {
			i, ai, bi = i+1, ai+1, bi+1
			continue
		}
		h := Hunk{OldStart: ai, NewStart: bi}
		for ; i < len(ops) && ops[i] != ' '; i++ {
			if ops[i] == '-' {
				h.OldCount, ai = h.OldCount+1, ai+1
			} else {
				h.NewCount, bi = h.NewCount+1, bi+1
			}
		}
		out = append(out, h)
	}
	return out
}

// TakeNew is a with hunk h replaced by what b has there: the hunk accepted.
func TakeNew(a, b []string, h Hunk) string {
	return strings.Join(a[:h.OldStart], "") + strings.Join(b[h.NewStart:h.NewStart+h.NewCount], "") +
		strings.Join(a[h.OldStart+h.OldCount:], "")
}

// TakeOld is b with hunk h replaced by what a has there: the hunk undone.
func TakeOld(a, b []string, h Hunk) string {
	return strings.Join(b[:h.NewStart], "") + strings.Join(a[h.OldStart:h.OldStart+h.OldCount], "") +
		strings.Join(b[h.NewStart+h.NewCount:], "")
}
