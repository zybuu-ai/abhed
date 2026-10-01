package ui

import (
	"strings"
	"testing"
)

func TestClusterWidths(t *testing.T) {
	for s, w := range map[string]int{
		"a": 1, "é": 1, "é": 1, "你": 2, "🙂": 2, "👩\u200d👩\u200d👧": 2,
		"🇮🇳": 2, "❤️": 2, "1️⃣": 2, "\x1b[31mred\x1b[0m": 3, "": 0,
	} {
		if got := displayWidth(s); got != w {
			t.Errorf("width(%q) = %d, want %d", s, got, w)
		}
	}
}

func TestClusterBoundaries(t *testing.T) {
	rs := []rune("aé👩\u200d👩\u200d👧🇮🇳🇺🇸x")
	var got []string
	for i := 0; i < len(rs); {
		e := clusterEnd(rs, i)
		got = append(got, string(rs[i:e]))
		if s := clusterStart(rs, e); s != i {
			t.Errorf("clusterStart(%d) = %d, want %d", e, s, i)
		}
		i = e
	}
	want := []string{"a", "é", "👩\u200d👩\u200d👧", "🇮🇳", "🇺🇸", "x"}
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
	if got := sanitize("👩\u200d👩\u200d👧", false); got != "👩\u200d👩\u200d👧" {
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
	if got := reveal("👩\u200d👧 ok\nnext"); got != "👩\u200d👧 ok\nnext" {
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

// A joiner is kept only inside an emoji; between letters it would make two
// strings that look the same, so reveal shows it.
func TestRevealMarksAJoinerOutsideEmoji(t *testing.T) {
	if got := reveal("中\u200d文"); !strings.Contains(got, "⟨U+200D⟩") {
		t.Fatalf("a joiner between letters was hidden: %q", got)
	}
	if got := sanitize("a\u200db", false); strings.ContainsRune(got, 0x200d) {
		t.Fatalf("sanitize kept a joiner between letters: %q", got)
	}
	for _, emoji := range []string{"👩\u200d👩\u200d👧", "👨🏽\u200d🦰", "❤️\u200d🔥", "🏳️\u200d🌈"} {
		if got := reveal(emoji); got != emoji {
			t.Errorf("reveal changed %q to %q", emoji, got)
		}
	}
}

// Conceal (SGR 8) is dropped from printed text; colours that happen to hold
// an 8, and the rest of the token, are kept.
func TestSanitizeDropsConceal(t *testing.T) {
	for in, want := range map[string]string{
		"a\x1b[8mhidden\x1b[0m":      "ahidden\x1b[0m",
		"a\x1b[1;8;31mb":             "a\x1b[1;31mb",
		"a\x1b[08mb":                 "ab",
		"a\x1b[38;5;8mb":             "a\x1b[38;5;8mb",
		"a\x1b[48;2;8;8;8;8mb":       "a\x1b[48;2;8;8;8mb",
		"a\x1b[38:5:8mb":             "a\x1b[38:5:8mb",
		"a\x1b[1mb\x1b[28mc":         "a\x1b[1mb\x1b[28mc",
		"a\x1b[38;5;196;8;4mb\x1b[m": "a\x1b[38;5;196;4mb\x1b[m",
	} {
		if got := sanitize(in, true); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// Printed output cannot set its text to its background's colour when both
// are explicit, in one SGR or across several, in any colour notation. Other
// colours, and a colour on the default background, pass.
func TestColourGuardDropsTextOnItsOwnColour(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b[31;41mhid":                        "hid",
		"\x1b[41m\x1b[31mhid":                   "\x1b[41mhid",
		"\x1b[38;5;1;48;5;1mhid":                "hid",
		"\x1b[31m\x1b[48;5;1mhid":               "\x1b[31mhid",
		"\x1b[38;2;9;9;9m\x1b[48;2;9;9;9mhid":   "\x1b[38;2;9;9;9mhid",
		"\x1b[38:2::9:9:9m\x1b[48:2::9:9:9mhid": "\x1b[38:2::9:9:9mhid",
		"\x1b[97;107mhid":                       "hid",
		"\x1b[38;5;15;107mhid":                  "hid",
		"\x1b[31;42mok":                         "\x1b[31;42mok",
		"\x1b[31mok":                            "\x1b[31mok",
		"\x1b[31;41m\x1b[0m\x1b[41mok":          "\x1b[0m\x1b[41mok",
		"\x1b[41m\x1b[0m\x1b[31mok":             "\x1b[41m\x1b[0m\x1b[31mok",
		"\x1b[41m\x1b[39;49m\x1b[31mok":         "\x1b[41m\x1b[39;49m\x1b[31mok",
	} {
		var g colourGuard
		if got := g.filter(in); got != want {
			t.Errorf("filter(%q) = %q, want %q", in, got, want)
		}
	}
	// The colours carry across writes, as they do on the terminal.
	var g colourGuard
	if got := g.filter("\x1b[44mblue ") + g.filter("\x1b[34mhid"); got != "\x1b[44mblue hid" {
		t.Errorf("across writes: %q", got)
	}
	// Both captured paths use it: the dock and the line mode's filter.
	f := &streamFilter{keepSGR: true}
	if got := f.feed([]byte("\x1b[32;42mx\n")) + f.flush(); got != "x\n" {
		t.Errorf("line mode: %q", got)
	}
}
