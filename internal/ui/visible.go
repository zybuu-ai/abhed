package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// hiddenWarning is shown with a prompt whose text had characters made visible.
const hiddenWarning = "! this call contains hidden or control characters"

// Visible returns s with every character that could change what a terminal
// shows, rather than print as itself, written out as a visible escape: C0
// controls other than newline and tab, DEL, C1 controls, invalid UTF-8, line
// and paragraph separators, every format character (zero-width, bidi, tag)
// and characters that draw nothing. A long run of spaces or tabs, which could
// push the rest of a line out of view, is written as a count: ⟨32 spaces⟩.
// Nothing is dropped, so what is approved is what is displayed.
func Visible(s string) string { return visible(s, true, true) }

// VisibleLine is Visible for a one-line field: a newline is escaped too, so a
// value cannot start a line that looks like part of the prompt.
func VisibleLine(s string) string { return visible(s, false, true) }

// VisibleOutput is Visible for a tool's output, where aligned columns are
// expected: runs of spaces and tabs are left as they are.
func VisibleOutput(s string) string { return visible(s, true, false) }

// HasHidden reports whether Visible would change s: it carries a character
// that does not print as itself. A newline alone does not count.
func HasHidden(s string) bool { return !isPlain(s, true, true) }

// ArgsHidden reports whether any key or string value in a call's arguments,
// at any depth, has a hidden character, including fields no prompt draws.
func ArgsHidden(raw json.RawMessage) bool {
	var v any
	if !utf8.Valid(raw) || json.Unmarshal(raw, &v) != nil {
		return HasHidden(string(raw))
	}
	return valueHidden(v, 0)
}

// valueHidden also reads a string that is itself JSON, such as a manifest,
// where an escape like \u001b is a control character once decoded.
func valueHidden(v any, nested int) bool {
	switch x := v.(type) {
	case string:
		if HasHidden(x) {
			return true
		}
		if t := strings.TrimSpace(x); nested < 3 && t != "" && (t[0] == '{' || t[0] == '[') {
			var inner any
			if json.Unmarshal([]byte(t), &inner) == nil {
				return valueHidden(inner, nested+1)
			}
		}
	case []any:
		for _, e := range x {
			if valueHidden(e, nested) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if HasHidden(k) || valueHidden(e, nested) {
				return true
			}
		}
	}
	return false
}

func visible(s string, keepNewline, runs bool) string {
	if isPlain(s, keepNewline, runs) {
		return s
	}
	var b strings.Builder
	var prev rune
	for i := 0; i < len(s); {
		if c := s[i]; c == ' ' || c == '\t' {
			end, mark := blankRun(s, i)
			if runs && mark != "" {
				b.WriteString(mark)
			} else {
				b.WriteString(s[i:end])
			}
			i, prev = end, rune(s[end-1])
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n <= 1 {
			fmt.Fprintf(&b, `\x%02x`, s[i])
			i++
			prev = r
			continue
		}
		i += n
		switch {
		case r == '\n' && keepNewline:
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			b.WriteString(controlEscape(r))
		case needsEscape(r, prev):
			fmt.Fprintf(&b, "⟨U+%04X⟩", r)
		default:
			b.WriteRune(r)
		}
		prev = r
	}
	return b.String()
}

func isPlain(s string, keepNewline, runs bool) bool {
	var prev rune
	for i := 0; i < len(s); {
		if c := s[i]; c == ' ' || c == '\t' {
			end, mark := blankRun(s, i)
			if runs && mark != "" {
				return false
			}
			i, prev = end, rune(s[end-1])
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n <= 1 {
			return false
		}
		i += n
		if r == '\n' && keepNewline {
			prev = r
			continue
		}
		if r < 0x20 || r == 0x7f || needsEscape(r, prev) {
			return false
		}
		prev = r
	}
	return true
}

// blankRun reads the spaces and tabs from s[i], returning where they end and,
// for a run of eight columns or more (a tab counts eight), a marker to show
// instead. Indentation at the start of a line is marked only from 32 columns.
func blankRun(s string, i int) (int, string) {
	j, sp, tb := i, 0, 0
	for ; j < len(s) && (s[j] == ' ' || s[j] == '\t'); j++ {
		if s[j] == ' ' {
			sp++
		} else {
			tb++
		}
	}
	limit := 8
	if i > 0 && s[i-1] == '\n' {
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

// needsEscape reports a rune that does not print as itself. U+FE0F asks for
// emoji style, so it is shown raw only after a symbol it can apply to.
func needsEscape(r, prev rune) bool {
	switch r {
	case 0x034f, 0x115f, 0x1160, 0x2800, 0x3164, 0xffa0:
		return true
	case 0xfe0f:
		return !emojiBase(prev)
	}
	return r >= 0x80 && r <= 0x9f || r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r)
}

func emojiBase(r rune) bool {
	return unicode.In(r, unicode.So, unicode.Sm) || r >= '0' && r <= '9' || r == '#' || r == '*' ||
		r == 0x203c || r == 0x2049 || r == 0x2139
}

func controlEscape(r rune) string {
	switch r {
	case '\a':
		return `\a`
	case '\b':
		return `\b`
	case '\f':
		return `\f`
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\v':
		return `\v`
	}
	return fmt.Sprintf(`\x%02x`, r)
}

// visibleTracker makes each field visible and remembers whether any changed,
// so a prompt can say it is showing characters the model tried to hide.
type visibleTracker struct{ hidden bool }

// line escapes newlines too, but a newline alone does not raise the warning:
// a multi-line command hides nothing once it is shown as \n.
func (v *visibleTracker) line(s string) string {
	if HasHidden(s) {
		v.hidden = true
	}
	return VisibleLine(s)
}
