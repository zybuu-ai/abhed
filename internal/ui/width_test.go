package ui

import (
	"strings"
	"testing"
)

func TestClusterWidths(t *testing.T) {
	for s, w := range map[string]int{
		"a": 1, "é": 1, "é": 1, "你": 2, "🙂": 2, "👩‍👩‍👧": 2,
		"🇮🇳": 2, "❤️": 2, "1️⃣": 2, "\x1b[31mred\x1b[0m": 3, "": 0,
	} {
		if got := displayWidth(s); got != w {
			t.Errorf("width(%q) = %d, want %d", s, got, w)
		}
	}
}

func TestClusterBoundaries(t *testing.T) {
	rs := []rune("aé👩‍👩‍👧🇮🇳🇺🇸x")
	var got []string
	for i := 0; i < len(rs); {
		e := clusterEnd(rs, i)
		got = append(got, string(rs[i:e]))
		if s := clusterStart(rs, e); s != i {
			t.Errorf("clusterStart(%d) = %d, want %d", e, s, i)
		}
		i = e
	}
	want := []string{"a", "é", "👩‍👩‍👧", "🇮🇳", "🇺🇸", "x"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", got)
	}
}

// Wrapping never cuts a word unless the word alone is wider than a row.
func TestWrapWordsKeepsWordsWhole(t *testing.T) {
	text := "Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor"
	for _, w := range []int{12, 17, 23, 40} {
		rows := wrapWords(text, w)
		for _, r := range rows {
			if displayWidth(r) > w {
				t.Errorf("width %d: row %q too wide", w, r)
			}
		}
		if got := strings.Join(rows, " "); got != text {
			t.Errorf("width %d: rejoined %q", w, got)
		}
	}
	rows := wrapWords("a supercalifragilisticexpialidocious word", 10)
	for _, r := range rows {
		if displayWidth(r) > 10 {
			t.Errorf("row %q too wide", r)
		}
	}
}

// Text from a model, a tool or a status command cannot move the cursor,
// set the clipboard or retitle the window.
func TestSanitizeStripsControlSequences(t *testing.T) {
	in := "ok \x1b[31mred\x1b[0m \x1b]52;c;ZXZpbA==\x07\x1b]0;title\x07\x1b[2J\x1b[H\r\x08\u202eevil"
	got := sanitize(in, true)
	if strings.Contains(got, "]52") || strings.Contains(got, "]0;") || strings.Contains(got, "[2J") ||
		strings.Contains(got, "[H") || strings.ContainsAny(got, "\r\x08\u202e") {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(got, "\x1b[31mred\x1b[0m") {
		t.Fatalf("styling was lost: %q", got)
	}
	if strings.Contains(sanitize(in, false), "\x1b") {
		t.Fatal("escapes kept without keepSGR")
	}
}

func TestHardWrapCarriesStyle(t *testing.T) {
	rows := hardWrap("\x1b[1mabcdef\x1b[0m", 3)
	if len(rows) != 2 || !strings.HasPrefix(rows[1], "\x1b[1m") {
		t.Fatalf("got %q", rows)
	}
}

// Every rune is checked, not only the first of a character: a zero-width
// joiner could otherwise carry a carriage return, a backspace, a bell, a C1
// control or a bidi override past the filter.
func TestSanitizeChecksEveryRune(t *testing.T) {
	for _, in := range []string{
		"x\u200d\r", "x\u200d\b", "x\u200d\a", "x\u200d\u009b31m", "x\u200d\u202e", "x\u200d\u009d0;t\a",
		"a\u200bb", "a\u2060b", "a\ufeffb", "a\U000e0041b", "a\u2028b", "a\u200eb", "a\u061cb", "a\u2066b",
	} {
		out := sanitize(in, true)
		for _, r := range out {
			if hiddenRune(r) || r == 0x200d {
				t.Errorf("sanitize(%q) kept %U: %q", in, r, out)
			}
		}
	}
	// An emoji keeps its joiners.
	if got := sanitize("👩‍👩‍👧", false); got != "👩‍👩‍👧" {
		t.Errorf("emoji changed: %q", got)
	}
}

// What a person approves shows what is hidden, as marked escapes.
func TestRevealShowsHiddenRunes(t *testing.T) {
	got := reveal("touch pwned #\u200d\r│ $ ls -la\u200b\x1b]52;c;x\a")
	for _, want := range []string{"touch pwned #", `⟨U+200D⟩`, `⟨\r⟩`, "│ $ ls -la", `⟨U+200B⟩`, `⟨\e⟩]52;c;x⟨\a⟩`} {
		if !strings.Contains(got, want) {
			t.Errorf("reveal lacks %q: %q", want, got)
		}
	}
	for _, r := range got {
		if hiddenRune(r) {
			t.Fatalf("reveal left %U in %q", r, got)
		}
	}
	if got := reveal("👩‍👧 ok\nnext"); got != "👩‍👧 ok\nnext" {
		t.Errorf("reveal changed plain text: %q", got)
	}
}

// The line mode's output filter holds an escape or a character split across
// reads until it is whole, so nothing leaks and nothing is garbled.
func TestStreamFilterAcrossReads(t *testing.T) {
	in := "héllo \x1b]52;c;U1BPT0Y=\x07wörld \x1b[2J你好\x1b[31mred\x1b[0m\n"
	for cut := 1; cut < len(in); cut++ {
		f := &streamFilter{keepSGR: true}
		got := f.feed([]byte(in[:cut])) + f.feed([]byte(in[cut:])) + f.flush()
		if got != "héllo wörld 你好\x1b[31mred\x1b[0m\n" {
			t.Fatalf("cut at %d: %q", cut, got)
		}
	}
}
