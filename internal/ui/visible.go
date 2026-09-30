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
// and paragraph separators and every format character (zero-width, bidi, tag).
// Nothing is dropped, so what is approved is what is displayed.
func Visible(s string) string { return visible(s, true) }

// VisibleLine is Visible for a one-line field: a newline is escaped too, so a
// value cannot start a line that looks like part of the prompt.
func VisibleLine(s string) string { return visible(s, false) }

// HasHidden reports whether Visible would change s: it carries a character
// that does not print as itself. A newline alone does not count.
func HasHidden(s string) bool { return !isPlain(s, true) }

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

func visible(s string, keepNewline bool) string {
	if isPlain(s, keepNewline) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n <= 1 {
			fmt.Fprintf(&b, `\x%02x`, s[i])
			i++
			continue
		}
		i += n
		switch {
		case r == '\t', r == '\n' && keepNewline:
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			b.WriteString(controlEscape(r))
		case needsEscape(r):
			fmt.Fprintf(&b, "⟨U+%04X⟩", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isPlain(s string, keepNewline bool) bool {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n <= 1 {
			return false
		}
		i += n
		if r == '\t' || r == '\n' && keepNewline {
			continue
		}
		if r < 0x20 || r == 0x7f || needsEscape(r) {
			return false
		}
	}
	return true
}

func needsEscape(r rune) bool {
	return r >= 0x80 && r <= 0x9f || r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r)
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
