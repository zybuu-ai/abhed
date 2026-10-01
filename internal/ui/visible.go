package ui

import "strings"

// Visible is reveal for callers outside the package: every character that
// would not print as itself is shown as a marked escape, and nothing is dropped.
func Visible(s string) string { return reveal(s) }

// VisibleLine is Visible for a one-line field: a newline is shown too, so a
// value cannot start a line that looks like part of the prompt.
func VisibleLine(s string) string { return strings.ReplaceAll(reveal(s), "\n", `⟨\n⟩`) }

// HasHidden reports whether s carries a character Visible shows as an
// escape. A newline or a tab alone does not count.
func HasHidden(s string) bool { return reveal(s) != strings.ReplaceAll(s, "\t", "    ") }
