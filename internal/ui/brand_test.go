package ui

import (
	"strings"
	"testing"
)

// The facts sit beside the mark, so every mark row has to be the same width
// or the right-hand column staggers.
func TestBannerKeepsTheFactsAligned(t *testing.T) {
	out := Banner(Style{}, "v1", "model-x", "~/w", "process", "memory")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(markRows) {
		t.Fatalf("banner has %d lines, the mark has %d rows", len(lines), len(markRows))
	}
	col := -1
	for i, line := range lines {
		r := []rune(line)
		if i == 0 {
			// The first fact starts where the mark and its gutter end.
			col = len([]rune("  "+strings.Split(MarkLarge, "\n")[0])) + 3
		}
		if len(r) > col && r[col-1] != ' ' {
			t.Errorf("line %d: the facts column does not start at rune %d: %q", i, col, line)
		}
	}
	for i, row := range strings.Split(MarkLarge, "\n") {
		if n := len([]rune(row)); n != len([]rune(strings.Split(MarkLarge, "\n")[0])) {
			t.Errorf("mark row %d is %d runes wide, row 0 is %d", i, n, len([]rune(strings.Split(MarkLarge, "\n")[0])))
		}
	}
}

func TestAccentIsTheBrandOrange(t *testing.T) {
	if got := (Style{enabled: true}).Accent("x"); got != "\x1b[38;5;208mx\x1b[0m" {
		t.Errorf("Accent = %q", got)
	}
	if got := (Style{}).Accent("x"); got != "x" {
		t.Errorf("Accent without colour = %q, want plain text", got)
	}
}
