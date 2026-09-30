package ui

import (
	"io"
	"os"
)

// Style applies the terminal styling, disabled when not writing to a terminal
// or when NO_COLOR is set.
type Style struct{ enabled bool }

func NewStyle(w io.Writer) Style {
	if os.Getenv("NO_COLOR") != "" {
		return Style{false}
	}
	// A writer that stands in for the terminal answers for itself. Without
	// this, wrapping os.Stdout in anything at all silently turned colour off,
	// because the check could only recognise an *os.File.
	if t, ok := w.(interface{ IsTerminal() bool }); ok {
		return Style{t.IsTerminal()}
	}
	f, isFile := w.(*os.File)
	if !isFile {
		return Style{false}
	}
	info, err := f.Stat()
	if err != nil {
		return Style{false}
	}
	return Style{(info.Mode() & os.ModeCharDevice) != 0}
}

// Enabled reports whether styling is on.
func (s Style) Enabled() bool { return s.enabled }

func (s Style) wrap(code, text string) string {
	if !s.enabled || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s Style) Dim(t string) string     { return s.wrap("2", t) }
func (s Style) Bold(t string) string    { return s.wrap("1", t) }
func (s Style) Italic(t string) string  { return s.wrap("3", t) }
func (s Style) Strike(t string) string  { return s.wrap("9", t) }
func (s Style) Red(t string) string     { return s.wrap("31", t) }
func (s Style) Green(t string) string   { return s.wrap("32", t) }
func (s Style) Yellow(t string) string  { return s.wrap("33", t) }
func (s Style) Blue(t string) string    { return s.wrap("34", t) }
func (s Style) Magenta(t string) string { return s.wrap("35", t) }
func (s Style) Cyan(t string) string    { return s.wrap("36", t) }

// Code is inline code in prose.
func (s Style) Code(t string) string { return s.wrap("38;5;208", t) }

// Accent is the brand orange: 256-colour 202 (#FF5F00) is the nearest to it
// and reads on dark terminals and, more faintly, on white.
func (s Style) Accent(t string) string { return s.wrap("38;5;202", t) }

// Reverse swaps foreground and background, which is how a selected row in a
// list reads as selected on every terminal theme — a colour chosen for a dark
// background disappears on a light one.
func (s Style) Reverse(t string) string { return s.wrap("7", t) }

// DiffAdd and DiffDel colour a diff's added and removed lines.
func (s Style) DiffAdd(t string) string { return s.wrap("32", t) }
func (s Style) DiffDel(t string) string { return s.wrap("31", t) }
