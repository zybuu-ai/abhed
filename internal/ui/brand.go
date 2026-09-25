// Package ui renders Abhed's terminal surface.
package ui

import (
	"fmt"
	"strings"
)

// Abhed's mark is an isometric A: a solid frame with an orange strand running up
// its right side and across its middle, and the small orange square of the
// wordmark beside it. The terminal draws it in two colours to keep that reading.

// markLine is one row of the startup mark: pairs of text and whether it is the
// orange part.
type markLine []struct {
	text   string
	orange bool
}

// markRows is the startup mark, seven rows so the facts beside it end together.
var markRows = []markLine{
	{{"          ", false}, {"▄", true}, {"   ", false}},
	{{"     ▗▟", false}, {"▙▖", true}, {"     ", false}},
	{{"    ▗█▛", false}, {"▜█▖", true}, {"    ", false}},
	{{"   ▗█▛  ", false}, {"▜█▖", true}, {"   ", false}},
	{{"  ▗█▛", false}, {"▀▀▀▀", true}, {"▜█▖  ", false}},
	{{" ▗█▛      ▜█▖ ", false}},
	{{"▗██▖      ▗██▖", false}},
}

// MarkLarge is the startup mark as plain text, for places that cannot colour it.
var MarkLarge = func() string {
	var lines []string
	for _, row := range markRows {
		var b strings.Builder
		for _, seg := range row {
			b.WriteString(seg.text)
		}
		lines = append(lines, b.String())
	}
	return strings.Join(lines, "\n")
}()

// Glyph is the single-character form for prompts and log lines.
const Glyph = "▲"

// Banner renders the startup identity block.
//
// Deliberately restrained: an engineer sees this on every invocation, and a
// banner that entertains on the first run irritates on the hundredth. It earns
// its space by carrying the four facts that change between runs — model,
// workspace, sandbox tier, storage — not by being decorative.
func Banner(s Style, version, model, workspace, sandbox, storage string) string {
	var b strings.Builder

	col := strings.Split(MarkLarge, "\n")
	// Facts sit beside the mark rather than beneath it, so the block stays
	// seven lines instead of twelve.
	// Seven rows, matching the mark's height so both columns end together.
	// Model and sandbox are what change between runs and what a reader checks;
	// the rest is one line of identity.
	rows := []string{
		s.Bold("ABHED") + "  " + s.Dim(version),
		s.Dim("deep agent harness · on-prem"),
		"",
		s.Dim("model    ") + model,
		s.Dim("work     ") + workspace,
		s.Dim("sandbox  ") + sandbox,
		s.Dim("storage  ") + storage,
	}

	// Pad by RUNE count: the block characters are multi-byte, so %-14s would
	// align on bytes and stagger the right-hand column.
	width := 0
	for _, line := range col {
		if n := len([]rune(line)); n > width {
			width = n
		}
	}
	for i, row := range markRows {
		right := ""
		if i < len(rows) {
			right = rows[i]
		}
		var mark strings.Builder
		for _, seg := range row {
			if seg.orange {
				mark.WriteString(s.Accent(seg.text))
			} else {
				mark.WriteString(s.Bold(seg.text))
			}
		}
		pad := strings.Repeat(" ", width-len([]rune(col[i])))
		fmt.Fprintf(&b, "  %s%s   %s\n", mark.String(), pad, right)
	}
	return b.String()
}

// Prompt is the input marker: the glyph, not a bare chevron.
func Prompt(s Style) string {
	return s.Accent(Glyph) + " "
}

// Rule draws a labelled separator, used between turns so a long session stays
// scannable.
func Rule(s Style, label string, width int) string {
	if width <= 0 {
		width = 72
	}
	if label == "" {
		return s.Dim(strings.Repeat("─", width))
	}
	head := "── " + label + " "
	if n := width - len([]rune(head)); n > 0 {
		head += strings.Repeat("─", n)
	}
	return s.Dim(head)
}
