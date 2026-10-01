package ui

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The diff's two sides are exactly the old and new texts, and line numbers
// count each side.
func TestLineDiffIsAnEditScript(t *testing.T) {
	seed := uint64(7)
	rnd := func(n int) int {
		seed = seed*6364136223846793005 + 1442695040888963407
		return int((seed >> 33) % uint64(n))
	}
	gen := func() []string {
		var l []string
		for i := rnd(12); i > 0; i-- {
			l = append(l, string(rune('a'+rnd(4))))
		}
		return l
	}
	for i := 0; i < 500; i++ {
		a, b := gen(), gen()
		var gotA, gotB []string
		na, nb := 0, 0
		for _, op := range lineDiff(a, b) {
			switch op.kind {
			case ' ':
				gotA, gotB = append(gotA, op.text), append(gotB, op.text)
				na, nb = na+1, nb+1
				if op.oldNum != na || op.newNum != nb {
					t.Fatalf("%v→%v: numbers %d,%d want %d,%d", a, b, op.oldNum, op.newNum, na, nb)
				}
			case '-':
				gotA = append(gotA, op.text)
				na++
				if op.oldNum != na {
					t.Fatalf("%v→%v: old number %d want %d", a, b, op.oldNum, na)
				}
			case '+':
				gotB = append(gotB, op.text)
				nb++
				if op.newNum != nb {
					t.Fatalf("%v→%v: new number %d want %d", a, b, op.newNum, nb)
				}
			}
		}
		if strings.Join(gotA, ",") != strings.Join(a, ",") || strings.Join(gotB, ",") != strings.Join(b, ",") {
			t.Fatalf("%v→%v: sides %v / %v", a, b, gotA, gotB)
		}
	}
}

// A diff shows each change with three lines around it, line numbers, and
// the lines between changes elided.
func TestFileDiffRows(t *testing.T) {
	var before, after []string
	for i := 1; i <= 30; i++ {
		before = append(before, fmt.Sprintf("line %d", i))
	}
	after = append(after, before...)
	after[4] = "line five"
	after = append(after[:20], append([]string{"inserted"}, after[20:]...)...)
	d := newFileDiff("f.txt", strings.Join(before, "\n")+"\n", strings.Join(after, "\n")+"\n", false)
	if d.added != 2 || d.removed != 1 {
		t.Fatalf("+%d -%d", d.added, d.removed)
	}
	rows := d.rows(Style{}, 60, 0)
	text := strings.Join(rows, "\n")
	for _, want := range []string{" 5 - line 5", " 5 + line five", " 2   line 2", "21 + inserted", "⋯"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "line 12") {
		t.Errorf("unchanged lines far from a change are shown:\n%s", text)
	}
	if got := d.summary(); got != "Updated f.txt with 2 additions and 1 removal" {
		t.Errorf("summary %q", got)
	}
}

// Long diff lines wrap under their gutter: at 40 columns nothing is cut.
func TestFileDiffWrapsAtNarrowWidths(t *testing.T) {
	long := "a line that is certainly longer than forty columns, END"
	d := newFileDiff("f", "x\n", long+"\n", false)
	rows := d.rows(Style{}, 38, 0)
	for _, r := range rows {
		if displayWidth(r) > 38 {
			t.Errorf("row %q wider than 38", r)
		}
	}
	if !strings.Contains(strings.Join(rows, ""), "END") {
		t.Fatalf("the end of the line was cut: %q", rows)
	}
}

// A long output shows its first and last lines and how many are between;
// the expanded view shows all.
func TestResultPreviewAndExpand(t *testing.T) {
	var body []string
	for i := 1; i <= 3000; i++ {
		body = append(body, fmt.Sprint(i))
	}
	b := &resultBlock{body: body, headN: 3, tailN: 2}
	rows := strings.Join(b.lines(80, Style{}, false), "\n")
	for _, want := range []string{"⎿  1", "2", "4", "… +2994 lines (ctrl+o to expand)", "2999", "3000"} {
		if !strings.Contains(rows, want) {
			t.Errorf("preview lacks %q:\n%s", want, rows)
		}
	}
	if strings.Contains(rows, "\n     1500") {
		t.Errorf("the middle is shown in the preview")
	}
	if n := len(b.lines(80, Style{}, true)); n != 3000 {
		t.Errorf("expanded shows %d rows, want 3000", n)
	}
}

// A path named through a link to the workspace is still shown relative.
func TestRelPathThroughALink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if err := os.MkdirAll(filepath.Join(real, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := NewRenderer(io.Discard, false)
	r.SetWorkspace(link)
	for _, p := range []string{filepath.Join(link, "src", "a.go"), filepath.Join(real, "src", "a.go")} {
		if got := r.rel(p); got != filepath.Join("src", "a.go") {
			t.Errorf("rel(%q) = %q", p, got)
		}
	}
}

func TestRelPath(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	for in, want := range map[string]string{
		"/ws/src/main.go":   "src/main.go",
		"/ws":               ".",
		"/elsewhere/x":      "/elsewhere/x",
		"/home/me/notes.md": "~/notes.md",
		"rel/path":          "rel/path",
	} {
		if got := relPath("/ws", in); got != want {
			t.Errorf("relPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// Streaming a reply in any cut gives the same rows as rendering it whole.
func TestStreamedEqualsWhole(t *testing.T) {
	texts := []string{
		stubLongForTest,
		"# Title\n\nSome **bold** and `code` here.\n\n```py\nx = 1  # c\n```\n\n- a\n- b\n",
		"| a | b |\n|---|---|\n| 1 | 2 |\n\nafter",
	}
	s := Style{enabled: true}
	for _, text := range texts {
		want := renderMarkdown(s, text, 60)
		for _, cut := range []int{1, 3, 7, 50} {
			var m mdStream
			var got []string
			for i := 0; i < len(text); i += cut {
				c, _ := m.feed(s, text[i:min(i+cut, len(text))], 60)
				got = append(got, c...)
			}
			got = append(got, m.end(s, 60)...)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("cut %d: streamed\n%s\nwhole\n%s", cut, strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		}
	}
}

const stubLongForTest = "# Summary of changes\n\nHere is **what I found** in the `hello.txt` file and a few *notes*:\n\n" +
	"1. The greeting is fine.\n2. The trailing newline is missing.\n   - nested bullet one\n\n" +
	"```go\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n```\n\n> A quote.\n\nLorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua."
