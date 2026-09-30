package ui

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/ui/vt"
)

// The differ's output, applied to a terminal, always gives the frame asked
// for — whatever was on screen before.
func TestScreenDiffIsExact(t *testing.T) {
	term := vt.New(40, 12)
	s := newScreen(term)
	frames := [][]string{
		{"one", "two", "three"},
		{"one", "twx", "three"},
		{"one", "two"},
		{"\x1b[31mone\x1b[0m", "two", "three", "four"},
		{"\x1b[32mone\x1b[0m", "2", "three", "four"},
		{"你好世界", "🙂🙂", "éé"},
		{"你好", "🙂x🙂", "é"},
		{},
		{"back"},
	}
	for i, f := range frames {
		s.render(f, len(f)-1, 0)
		lines := term.Lines()
		for j, row := range f {
			if lines[j] != stripANSI(row) {
				t.Fatalf("frame %d row %d: screen %q, want %q\n%s", i, j, lines[j], stripANSI(row), term.Dump())
			}
		}
		for j := len(f); j < len(lines); j++ {
			if lines[j] != "" {
				t.Fatalf("frame %d: stale row %d %q\n%s", i, j, lines[j], term.Dump())
			}
		}
	}
}

// Committed lines scroll above the region and are never touched again.
func TestScreenCommitScrolls(t *testing.T) {
	term := vt.New(30, 6)
	s := newScreen(term)
	s.render([]string{"input", "footer"}, 0, 5)
	for i := 0; i < 10; i++ {
		s.commit([]string{"line " + string(rune('a'+i))}, []string{"input", "footer"}, 0, 5)
	}
	all := term.All()
	for i := 0; i < 10; i++ {
		if strings.Count(all, "line "+string(rune('a'+i))) != 1 {
			t.Fatalf("line %c not exactly once:\n%s", 'a'+i, all)
		}
	}
	if strings.Count(all, "input") != 1 || strings.Count(all, "footer") != 1 {
		t.Fatalf("the region was left behind:\n%s", all)
	}
	lines := term.Lines()
	if lines[len(lines)-2] != "input" || lines[len(lines)-1] != "footer" {
		t.Fatalf("region not at the bottom:\n%s", term.Dump())
	}
}
