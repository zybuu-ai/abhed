package ui

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Visible is reveal for callers outside the package, for a field a person
// approves or reads: every character that would not print as itself is shown
// as a marked escape, a long run of spaces or tabs as a count (⟨32 spaces⟩),
// and nothing is dropped, so what is approved is what is displayed.
func Visible(s string) string { return revealText(s, true, true) }

// VisibleLine is Visible for a one-line field: a newline is shown too, so a
// value cannot start a line that looks like part of the prompt.
func VisibleLine(s string) string { return revealText(s, false, true) }

// VisibleOutput is Visible for a tool's output, where aligned columns are
// expected: runs of spaces and tabs are not counted.
func VisibleOutput(s string) string { return reveal(s) }

// HasHidden reports whether Visible would change s beyond widening its tabs:
// it carries a character that does not print as itself, or a long run of
// blanks. A newline or a tab alone does not count.
func HasHidden(s string) bool { return Visible(s) != strings.ReplaceAll(s, "\t", "    ") }

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
		if t := strings.TrimSpace(x); nested < 4 && t != "" && (t[0] == '{' || t[0] == '[') {
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
