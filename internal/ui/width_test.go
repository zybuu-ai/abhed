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
