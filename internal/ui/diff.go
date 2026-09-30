package ui

import (
	"fmt"
	"strconv"
	"strings"
)

// diffOp is one line of a line diff.
type diffOp struct {
	kind   byte // ' ', '-', '+'
	text   string
	oldNum int // line number in the old text, for ' ' and '-'
	newNum int // line number in the new text, for ' ' and '+'
}

// splitLines splits text into lines, without the final newline's empty tail.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// lineDiff is the shortest edit script from a to b (Myers' O(ND) algorithm),
// with the common prefix and suffix taken off first, which is most of any
// real edit. Past a size where the search would be slow, the middle is
// shown as removed and added whole, which is still a correct diff.
func lineDiff(a, b []string) []diffOp {
	var ops []diffOp
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		ops = append(ops, diffOp{kind: ' ', text: a[pre], oldNum: pre + 1, newNum: pre + 1})
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]
	for _, op := range myers(ma, mb) {
		if op.oldNum > 0 {
			op.oldNum += pre
		}
		if op.newNum > 0 {
			op.newNum += pre
		}
		ops = append(ops, op)
	}
	for i := suf; i > 0; i-- {
		ai, bi := len(a)-i, len(b)-i
		ops = append(ops, diffOp{kind: ' ', text: a[ai], oldNum: ai + 1, newNum: bi + 1})
	}
	return ops
}

func myers(a, b []string) []diffOp {
	n, m := len(a), len(b)
	whole := func() []diffOp {
		var ops []diffOp
		for i, l := range a {
			ops = append(ops, diffOp{kind: '-', text: l, oldNum: i + 1})
		}
		for j, l := range b {
			ops = append(ops, diffOp{kind: '+', text: l, newNum: j + 1})
		}
		return ops
	}
	if n == 0 || m == 0 || n*m > 4_000_000 {
		return whole()
	}
	maxD := n + m
	off := maxD
	v := make([]int, 2*maxD+2)
	var trace [][]int
	for d := 0; d <= maxD; d++ {
		snap := make([]int, len(v))
		copy(snap, v)
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(a, b, trace, d, off)
			}
		}
	}
	return whole()
}

func backtrack(a, b []string, trace [][]int, d, off int) []diffOp {
	x, y := len(a), len(b)
	var rev []diffOp
	for ; d > 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[off+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			rev = append(rev, diffOp{kind: ' ', text: a[x], oldNum: x + 1, newNum: y + 1})
		}
		if x == prevX {
			y--
			rev = append(rev, diffOp{kind: '+', text: b[y], newNum: y + 1})
		} else {
			x--
			rev = append(rev, diffOp{kind: '-', text: a[x], oldNum: x + 1})
		}
	}
	for x > 0 && y > 0 {
		x--
		y--
		rev = append(rev, diffOp{kind: ' ', text: a[x], oldNum: x + 1, newNum: y + 1})
	}
	ops := make([]diffOp, len(rev))
	for i := range rev {
		ops[i] = rev[len(rev)-1-i]
	}
	return ops
}

// fileDiff is a change to one file, shown as hunks with context and line
// numbers.
type fileDiff struct {
	path           string
	ops            []diffOp
	added, removed int
	created        bool
}

func newFileDiff(path, before, after string, created bool) *fileDiff {
	f := &fileDiff{path: path, ops: lineDiff(splitLines(before), splitLines(after)), created: created}
	for _, op := range f.ops {
		switch op.kind {
		case '+':
			f.added++
		case '-':
			f.removed++
		}
	}
	return f
}

// summary says what changed, in words.
func (f *fileDiff) summary() string {
	plural := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + one + "s"
	}
	if f.created {
		return fmt.Sprintf("Created %s with %s", f.path, plural(f.added, "line"))
	}
	var parts []string
	if f.added > 0 {
		parts = append(parts, plural(f.added, "addition"))
	}
	if f.removed > 0 {
		parts = append(parts, plural(f.removed, "removal"))
	}
	if len(parts) == 0 {
		return "Updated " + f.path + " (no change)"
	}
	return "Updated " + f.path + " with " + strings.Join(parts, " and ")
}

// diffContext is how many unchanged lines frame each change.
const diffContext = 3

// rows renders the hunks: a gutter of line numbers, the sign, the line.
// Long lines wrap under their gutter rather than being cut. limit caps the
// rows shown, with a note of what was left; 0 shows all.
func (f *fileDiff) rows(s Style, width, limit int) []string {
	// Which ops are shown: every change, and diffContext lines around each.
	show := make([]bool, len(f.ops))
	for i, op := range f.ops {
		if op.kind != ' ' {
			for j := max(0, i-diffContext); j <= min(len(f.ops)-1, i+diffContext); j++ {
				show[j] = true
			}
		}
	}
	maxNum := 1
	for _, op := range f.ops {
		maxNum = max(maxNum, op.oldNum, op.newNum)
	}
	gw := len(strconv.Itoa(maxNum))
	textW := max(width-gw-3, 8)

	var out []string
	last := -1
	for i, op := range f.ops {
		if !show[i] {
			continue
		}
		if last >= 0 && i != last+1 {
			out = append(out, s.Dim(strings.Repeat(" ", gw)+" ⋯"))
		}
		last = i
		num := op.newNum
		if op.kind == '-' {
			num = op.oldNum
		}
		gutter := fmt.Sprintf("%*d", gw, num)
		body := strings.ReplaceAll(sanitize(op.text, false), "\t", "    ")
		for j, part := range hardWrap(body, textW) {
			g := gutter
			if j > 0 {
				g = strings.Repeat(" ", gw)
			}
			switch op.kind {
			case '+':
				out = append(out, s.Dim(g)+" "+s.DiffAdd("+ "+part))
			case '-':
				out = append(out, s.Dim(g)+" "+s.DiffDel("- "+part))
			default:
				out = append(out, s.Dim(g)+"   "+s.Dim(part))
			}
		}
	}
	if limit > 0 && len(out) > limit {
		more := len(out) - limit
		out = append(out[:limit], s.Dim(fmt.Sprintf("%s … +%d more lines (ctrl+o to expand)", strings.Repeat(" ", gw), more)))
	}
	return out
}
