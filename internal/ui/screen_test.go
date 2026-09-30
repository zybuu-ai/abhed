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

// Random frames that grow, shrink and change in the middle, as a streaming
// reply above a fixed input box does: whatever the differ sends, the screen
// always ends up showing the frame.
func TestScreenDiffRandomFrames(t *testing.T) {
	seed := uint64(1)
	rnd := func(n int) int {
		seed = seed*6364136223846793005 + 1442695040888963407
		return int((seed >> 33) % uint64(n))
	}
	pieces := []string{"", "a", "word", "\x1b[2m────────\x1b[0m", "\x1b[31mred\x1b[0m", "你好", "🙂 x", "input ▲", "footer"}
	for _, bottom := range []bool{false, true} {
		term := vt.New(30, 14)
		if bottom {
			// Start at the bottom of the screen, where growing scrolls.
			term.Write([]byte(strings.Repeat("filler\r\n", 13)))
		}
		s := newScreen(term)
		tail := []string{"── rule", "input ▲", "── rule", "footer"}
		for i := 0; i < 400; i++ {
			var f []string
			for j := rnd(6); j > 0; j-- {
				f = append(f, pieces[rnd(len(pieces))])
			}
			if rnd(3) > 0 {
				f = append(f, tail...)
			}
			s.render(f, max(len(f)-3, 0), rnd(5))
			lines := term.Lines()
			x, y := term.Cursor()
			_ = x
			top := y - max(len(f)-3, 0)
			if len(f) == 0 {
				top = y
			}
			for j, row := range f {
				if got := lines[top+j]; got != stripANSI(row) {
					t.Fatalf("bottom=%v frame %d row %d: screen %q, want %q\n%s", bottom, i, j, got, stripANSI(row), term.Dump())
				}
			}
			for j := top + len(f); j < len(lines); j++ {
				if lines[j] != "" {
					t.Fatalf("bottom=%v frame %d: stale row %d %q\n%s", bottom, i, j, lines[j], term.Dump())
				}
			}
		}
	}
}

// The screen writer itself is the boundary: a row handed to it with an
// escape, a control or a joiner-hidden CR is drawn as text and colour only,
// whatever composed it.
func TestScreenCleansEveryRow(t *testing.T) {
	var out strings.Builder
	s := newScreen(&out)
	row := "a\x1b]52;c;eA==\x07b\x1b[2Jc\u009b31md‍\re\x1b[31mred\x1b[0m"
	s.render([]string{row, "x"}, 0, 0)
	s.commit([]string{row}, []string{row}, 0, 0)
	assertClean(t, "the screen writer", out.String())
	if !strings.Contains(out.String(), "\x1b[31mred") {
		t.Fatalf("colour was lost: %q", out.String())
	}
}
