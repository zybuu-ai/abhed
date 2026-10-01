package server

import (
	"fmt"
	"strings"

	"github.com/zybuu-ai/abhed/internal/linediff"
)

const diffContext = 3

// unifiedDiff renders the change from before to after in unified format, and
// counts the lines added and removed.
func unifiedDiff(name, before, after string) (string, int, int) {
	a, b := linediff.SplitLines(before), linediff.SplitLines(after)
	ops := linediff.EditScript(a, b)

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
