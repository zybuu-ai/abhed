package server

import (
	"fmt"
	"strings"
)

const (
	diffContext = 3
	// maxEditDistance bounds the search. Its memory grows with the square of
	// the distance, and a file rewritten that thoroughly reads no better as a
	// minimal diff than as old text followed by new.
	maxEditDistance = 1000
)

// unifiedDiff renders the change from before to after in unified format, and
// counts the lines added and removed.
func unifiedDiff(name, before, after string) (string, int, int) {
	a, b := splitLines(before), splitLines(after)
	ops := editScript(a, b)

	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", name, name)
	added, removed := 0, 0
	for _, op := range ops {
		switch op {
		case '+':
			added++
		case '-':
			removed++
		}
	}

	// ai and bi are the next unread line on each side as ops is walked.
	ai, bi := 0, 0
	for i := 0; i < len(ops); {
		if ops[i] == ' ' {
			i, ai, bi = i+1, ai+1, bi+1
			continue
		}
		// A hunk runs from a change to the last change that has another within
		// two contexts of it, so neighbouring edits share their surroundings.
		end := i
		for j := i; j < len(ops) && j-end <= 2*diffContext+1; j++ {
			if ops[j] != ' ' {
				end = j
			}
		}
		lead := min(diffContext, i)
		tail := min(diffContext, len(ops)-1-end)
		start, stop := i-lead, end+tail+1

		var body strings.Builder
		oldStart, newStart := ai-lead, bi-lead
		oldCount, newCount := 0, 0
		x, y := oldStart, newStart
		for _, op := range ops[start:stop] {
			switch op {
			case ' ':
				writeDiffLine(&body, ' ', a[x])
				x, y, oldCount, newCount = x+1, y+1, oldCount+1, newCount+1
			case '-':
				writeDiffLine(&body, '-', a[x])
				x, oldCount = x+1, oldCount+1
			case '+':
				writeDiffLine(&body, '+', b[y])
				y, newCount = y+1, newCount+1
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(oldStart, oldCount), hunkRange(newStart, newCount))
		out.WriteString(body.String())
		i, ai, bi = stop, x, y
	}
	return out.String(), added, removed
}

// hunkRange formats a zero-based start and a count the way patch expects: an
// empty range names the line before it.
func hunkRange(start, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	return fmt.Sprintf("%d,%d", start+1, count)
}

func writeDiffLine(out *strings.Builder, op byte, line string) {
	out.WriteByte(op)
	out.WriteString(strings.TrimSuffix(line, "\n"))
	out.WriteByte('\n')
	if !strings.HasSuffix(line, "\n") {
		out.WriteString("\\ No newline at end of file\n")
	}
}

// splitLines keeps each line's terminator, so a file that gains or loses its
// final newline differs on that line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// editScript returns one op per line of the diff: ' ' kept, '-' removed from
// a, '+' added from b. The shortest script, by Myers' O(ND) algorithm.
func editScript(a, b []string) []byte {
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
	limit := min(n+m, maxEditDistance)
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
