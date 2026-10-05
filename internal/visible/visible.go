// Package visible is the one escaper for untrusted text a person reads or
// approves: every character that would not print as itself is shown as a
// marked escape (⟨\e⟩, ⟨U+202E⟩, ⟨\xff⟩), and nothing is dropped. The
// terminal UI and the configuration's prompts both use it.
package visible

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Hidden reports a rune that draws nothing readable or changes what is drawn around it: C0/C1
// controls, line and paragraph separators, and format characters (the emoji joiner is KeepJoiner's).
func Hidden(r rune) bool {
	switch {
	case r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0:
		return true
	case r == 0x2028 || r == 0x2029:
		return true
	}
	return unicode.Is(unicode.Cf, r)
}

// KeepJoiner reports whether the zero-width joiner at rs[i] joins two
// pictographs, as in a family emoji. Anywhere else it only hides what
// follows it, so it goes.
func KeepJoiner(rs []rune, i int) bool {
	if i == 0 || i+1 >= len(rs) {
		return false
	}
	left := rs[i-1]
	// An emoji's presentation selector or skin tone sits between it and
	// the joiner.
	for j := i - 1; j > 0 && (left == 0xfe0f || left >= 0x1f3fb && left <= 0x1f3ff); j-- {
		left = rs[j-1]
	}
	return pictograph(left) && pictograph(rs[i+1])
}

// pictograph reports an emoji's base: only between two of these does a
// joiner make one picture. Between letters — 中‍文 — it makes two strings
// that look the same, so it is shown or dropped.
func pictograph(r rune) bool {
	switch {
	case r >= 0x1f000 && r <= 0x1faff:
		return true
	case r >= 0x2600 && r <= 0x27bf, r >= 0x2300 && r <= 0x23ff, r >= 0x2b00 && r <= 0x2bff:
		return true
	case r == 0x00a9, r == 0x00ae, r == 0x203c, r == 0x2049, r == 0x2122, r == 0x2139:
		return true
	}
	return false
}

// Text shows every character that would not print as itself as a marked
// escape, and drops nothing. It has two choices: keepNewline leaves a newline as it
// is, else it is shown as ⟨\n⟩; runs writes a long run of spaces or tabs,
// which could push the rest of a line out of view, as a count: ⟨32 spaces⟩.
func Text(s string, keepNewline, runs bool) string {
	var b strings.Builder
	rs, bad := decodeRunes(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if bad[i] != 0 {
			fmt.Fprintf(&b, "⟨\\x%02x⟩", bad[i])
			continue
		}
		if r == ' ' || r == '\t' {
			end, mark := blankRun(rs, i)
			if runs && mark != "" {
				b.WriteString(mark)
			} else {
				b.WriteString(strings.ReplaceAll(string(rs[i:end]), "\t", "    "))
			}
			i = end - 1
			continue
		}
		var prev rune
		if i > 0 {
			prev = rs[i-1]
		}
		switch {
		case r == '\n' && keepNewline:
			b.WriteByte('\n')
		case r == '\n':
			b.WriteString("⟨\\n⟩")
		case r == 0x200d && KeepJoiner(rs, i):
			b.WriteRune(r)
		case r == '\r':
			b.WriteString("⟨\\r⟩")
		case r == '\b':
			b.WriteString("⟨\\b⟩")
		case r == 0x07:
			b.WriteString("⟨\\a⟩")
		case r == 0x1b:
			b.WriteString("⟨\\e⟩")
		case r == 0:
			b.WriteString("⟨\\0⟩")
		case Hidden(r) || r == 0x200d || drawsNothing(r, prev) || runs && otherSpace(r):
			fmt.Fprintf(&b, "⟨U+%04X⟩", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// decodeRunes splits s into runes, with the byte of each that is not UTF-8
// in bad (0 where the rune is whole), so it can be shown rather than become
// a replacement character.
func decodeRunes(s string) ([]rune, []byte) {
	rs := make([]rune, 0, len(s))
	bad := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n <= 1 {
			rs, bad = append(rs, utf8.RuneError), append(bad, s[i])
			i++
			continue
		}
		rs, bad = append(rs, r), append(bad, 0)
		i += n
	}
	return rs, bad
}

// drawsNothing reports a printable rune that shows as blank or as nothing:
// the Hangul fillers, the blank braille pattern, the combining grapheme
// joiner. U+FE0F asks for emoji style, so it is shown raw only after a
// symbol it can apply to.
func drawsNothing(r, prev rune) bool {
	switch r {
	case 0x034f, 0x115f, 0x1160, 0x2800, 0x3164, 0xffa0:
		return true
	case 0xfe0f:
		return !emojiBase(prev)
	}
	return false
}

func emojiBase(r rune) bool {
	return unicode.In(r, unicode.So, unicode.Sm) || r >= '0' && r <= '9' || r == '#' || r == '*' ||
		r == 0x203c || r == 0x2049 || r == 0x2139
}

// blankRun reads the spaces and tabs from rs[i], returning where they end
// and, for a run of eight columns or more (a tab counts eight), a marker to
// show instead. Indentation at the start of a line is marked only from 32
// columns.
func blankRun(rs []rune, i int) (int, string) {
	j, sp, tb := i, 0, 0
	for ; j < len(rs) && (rs[j] == ' ' || rs[j] == '\t'); j++ {
		if rs[j] == ' ' {
			sp++
		} else {
			tb++
		}
	}
	limit := 8
	if i > 0 && rs[i-1] == '\n' {
		limit = 32
	}
	if sp+tb < 2 || sp+8*tb < limit {
		return j, ""
	}
	return j, "⟨" + runCount(sp, tb) + "⟩"
}

func runCount(sp, tb int) string {
	part := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	switch {
	case tb == 0:
		return part(sp, "space", "spaces")
	case sp == 0:
		return part(tb, "tab", "tabs")
	}
	return part(sp, "space", "spaces") + ", " + part(tb, "tab", "tabs")
}

// otherSpace is a space other than ' ', such as U+00A0: in a command it reads
// as a space but splits no words, so a field a person approves shows it.
func otherSpace(r rune) bool { return r != ' ' && unicode.Is(unicode.Zs, r) }
