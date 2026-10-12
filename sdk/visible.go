package abhed

import (
	"encoding/json"

	"github.com/zybuu-ai/abhed/internal/ui"
)

// Visible is s as a person should see it before approving it: every
// character that would not print as itself (a bidi override, an ANSI escape,
// a zero-width space) is shown as a marked escape, and a long run of blanks
// as a count, so what is approved is what is displayed. The arguments passed
// to Approve and carried by events are the model's, raw: pass any of them a
// person reads through Visible, or VisibleLine for a one-line field.
func Visible(s string) string { return ui.Visible(s) }

// VisibleLine is Visible for a one-line field: a newline is shown too, so a
// value cannot start a line that looks like part of the prompt.
func VisibleLine(s string) string { return ui.VisibleLine(s) }

// ArgsHidden reports whether any key or string value in a call's arguments,
// at any depth, holds a character Visible would mark, including fields a
// prompt does not draw: an approver may refuse such a call outright.
func ArgsHidden(args json.RawMessage) bool { return ui.ArgsHidden(args) }
