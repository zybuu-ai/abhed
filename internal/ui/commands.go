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

// commands is the list /help, completion and the menu show, in /help's
// order. The command registry is its only source: it sets the list through
// SetCommands, and this package only displays it.
var commands []Command

var commandsMu sync.RWMutex

// SetCommands replaces the list /help, completion and the menu show. The
// registry that owns the commands calls it; this package only displays them.
func SetCommands(cs []Command) {
	commandsMu.Lock()
	defer commandsMu.Unlock()
	commands = slices.Clone(cs)
}

// commandList is the current list, safe to range over while it is replaced.
func commandList() []Command {
	commandsMu.RLock()
	defer commandsMu.RUnlock()
	return commands
}

// CommandList is the list SetCommands last gave.
func CommandList() []Command { return slices.Clone(commandList()) }

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
