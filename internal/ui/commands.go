package ui

import (
	"fmt"
	"strings"
)

// Command is one slash command: the name the user types, the argument shape,
// and a one-line description.
//
// One list drives three surfaces — the /help text, tab completion, and the
// suggestion menu that appears while typing. They were separate before, which
// is how /export came to exist in help and nowhere else.
type Command struct {
	Name string // "/mode"
	Args string // "<name>", "[hint]", ""
	Help string
}

// Commands is the full set, in the order /help prints them.
var Commands = []Command{
	{"/mode", "<name>", "default | accept-edits | plan | auto"},
	{"/undo", "", "revert the last turn's file changes"},
	{"/diff", "", "files changed this session"},
	{"/cost", "", "tokens, cache hit rate, compactions this session"},
	{"/compact", "[hint]", "compact the context now"},
	{"/clear", "", "clear the context, keep the workspace"},
	{"/memory", "", "show the ABHED.md files in effect"},
	{"/model", "[name]", "show or switch the model, keeping the conversation"},
	{"/sessions", "", "list recent sessions (durable store)"},
	{"/resume", "<id>", "replay a past session's transcript"},
	{"/tree", "", "show the session's steps, with the numbers /fork takes"},
	{"/fork", "[step]", "rebuild the conversation up to a step and continue from it"},
	{"/export", "[path]", "write the transcript (.html by default, .json for events)"},
	{"/hawkeye", "[path]", "what this session did: tokens, policy decisions, findings"},
	{"/think", "", "show or collapse the model's reasoning"},
	{"/cwd", "", "show the workspace root"},
	{"/help", "", "this list"},
	{"/quit", "", "exit"},
}

// HelpText renders the command list for /help.
func HelpText(s Style) string {
	var b strings.Builder
	for _, c := range Commands {
		left := c.Name
		if c.Args != "" {
			left += " " + c.Args
		}
		fmt.Fprintf(&b, "  %-18s %s\n", left, s.Dim(c.Help))
	}
	return strings.TrimRight(b.String(), "\n")
}

// MatchCommands returns the commands whose name starts with prefix.
//
// An empty prefix after "/" matches everything, which is what makes typing a
// bare "/" show the whole menu.
func MatchCommands(prefix string) []Command {
	if !strings.HasPrefix(prefix, "/") {
		return nil
	}
	var out []Command
	for _, c := range Commands {
		if strings.HasPrefix(c.Name, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// CommonPrefix returns the longest prefix shared by every candidate, so Tab
// completes as far as it unambiguously can rather than doing nothing when two
// commands share a stem (/co -> /co, but /com -> /compact).
func CommonPrefix(cs []Command) string {
	if len(cs) == 0 {
		return ""
	}
	p := cs[0].Name
	for _, c := range cs[1:] {
		for !strings.HasPrefix(c.Name, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	return p
}

// SuggestionMenu renders the candidate list shown under the prompt while a
// slash command is being typed. Capped, because a full-screen menu on a bare
// "/" pushes the conversation out of view.
func SuggestionMenu(s Style, cs []Command, max int) string {
	if len(cs) == 0 {
		return ""
	}
	more := 0
	if len(cs) > max {
		more = len(cs) - max
		cs = cs[:max]
	}
	var b strings.Builder
	for _, c := range cs {
		left := c.Name
		if c.Args != "" {
			left += " " + c.Args
		}
		fmt.Fprintf(&b, "  %s  %s\n", s.Cyan(fmt.Sprintf("%-18s", left)), s.Dim(c.Help))
	}
	if more > 0 {
		fmt.Fprintf(&b, "  %s\n", s.Dim(fmt.Sprintf("… %d more", more)))
	}
	return b.String()
}
