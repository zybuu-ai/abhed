package ui

import (
	"fmt"
	"slices"
	"strings"
	"sync"
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

// Commands is the full set, in the order /help prints them. It is the
// built-in list until the command registry replaces it through SetCommands;
// read it through commandList, since the registry may change it while the
// line editor is completing.
var Commands = []Command{
	{"/mode", "<name>", "default | accept-edits | plan | auto"},
	{"/undo", "", "revert the last turn's file changes"},
	{"/diff", "", "files changed this session"},
	{"/cost", "", "tokens, cache hit rate, compactions this session"},
	{"/compact", "[hint]", "compact the context now"},
	{"/clear", "", "start a new conversation and session, keep the workspace"},
	{"/tasks", "[cancel <id|all>]", "list background tasks, or cancel them"},
	{"/wake", "[off|notify|auto]", "show or set what a background result does while idle"},
	{"/memory", "", "show the ABHED.md files in effect"},
	{"/model", "[name]", "show or switch the model, keeping the conversation"},
	{"/sessions", "", "list recent sessions (durable store)"},
	{"/resume", "<id>", "replay a past session and continue its conversation"},
	{"/tree", "", "show the session's steps, with the numbers /fork takes"},
	{"/fork", "[step]", "rebuild the conversation up to a step and continue from it"},
	{"/export", "[path]", "write the transcript (.html by default, .json for events)"},
	{"/hawkeye", "[path]", "what this session did: tokens, policy decisions, findings"},
	{"/think", "", "show or collapse the model's reasoning"},
	{"/cwd", "", "show the workspace root"},
	{"/help", "", "this list"},
	{"/quit", "", "exit"},
}

var commandsMu sync.RWMutex

// SetCommands replaces the list /help, completion and the menu show. The
// registry that owns the commands calls it; this package only displays them.
func SetCommands(cs []Command) {
	commandsMu.Lock()
	defer commandsMu.Unlock()
	Commands = slices.Clone(cs)
}

// commandList is the current list, safe to range over while it is replaced.
func commandList() []Command {
	commandsMu.RLock()
	defer commandsMu.RUnlock()
	return Commands
}

// HelpText renders the command list for /help.
func HelpText(s Style) string {
	var b strings.Builder
	for _, c := range commandList() {
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
	for _, c := range commandList() {
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
		fmt.Fprintf(&b, "  %s  %s\n", s.Accent(fmt.Sprintf("%-18s", left)), s.Dim(c.Help))
	}
	if more > 0 {
		fmt.Fprintf(&b, "  %s\n", s.Dim(fmt.Sprintf("… %d more", more)))
	}
	return b.String()
}
