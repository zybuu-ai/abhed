package policy

import (
	"path"
	"regexp"
	"strings"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// flagForm is a bash rule's glob read as a program, its literal subcommands,
// the flags it names and a pattern for the rest, so a deny or ask rule also
// matches the flags moved or split: bash(rm -rf *) holds for rm x -rf and
// rm -r -f x. A program's own flag grammar is not known, so this is a
// superset reading, used only to add deny and ask matches.
type flagForm struct {
	prefix []string        // the program and literal words after it
	short  map[byte]bool   // short flags, from clusters such as -rf
	long   map[string]bool // long flags with no value, such as --force
	rest   *regexp.Regexp  // the remaining words, in order, joined by blanks
}

var shortCluster = regexp.MustCompile(`^-[A-Za-z0-9]+$`)

// longFlag reports a long option with no value glued to it.
func longFlag(w string) bool {
	return len(w) > 2 && strings.HasPrefix(w, "--") && !strings.Contains(w, "=")
}

// parseFlagForm reads glob as a flag form, or nil when it names no flag after
// literal words, or has a wildcard before its flags.
func parseFlagForm(glob string) *flagForm {
	fields := strings.Fields(glob)
	f := &flagForm{short: map[byte]bool{}, long: map[string]bool{}}
	i := 0
	for ; i < len(fields); i++ {
		w := fields[i]
		if strings.ContainsAny(w, "*?") {
			return nil
		}
		if strings.HasPrefix(w, "-") {
			break
		}
		f.prefix = append(f.prefix, w)
	}
	if len(f.prefix) == 0 {
		return nil
	}
flags:
	for ; i < len(fields); i++ {
		w := fields[i]
		switch {
		case shortCluster.MatchString(w):
			for j := 1; j < len(w); j++ {
				f.short[w[j]] = true
			}
		case longFlag(w):
			f.long[w] = true
		default:
			break flags
		}
	}
	if len(f.short)+len(f.long) == 0 {
		return nil
	}
	re, err := globToRegexp(strings.Join(fields[i:], " "))
	if err != nil {
		return nil
	}
	f.rest = re
	f.prefix[0] = tools.CommandName(f.prefix[0])
	return f
}

// matches reports a command whose program and leading words are the form's,
// whose flags anywhere before -- include the form's, and whose other words,
// in order, match the form's rest.
func (f *flagForm) matches(command string) bool {
	fields := strings.Fields(command)
	if len(fields) < len(f.prefix) || tools.CommandName(path.Base(fields[0])) != f.prefix[0] {
		return false
	}
	for k := 1; k < len(f.prefix); k++ {
		if fields[k] != f.prefix[k] {
			return false
		}
	}
	short, long := map[byte]bool{}, map[string]bool{}
	var operands []string
	ended := false
	for _, w := range fields[len(f.prefix):] {
		switch {
		case ended:
			operands = append(operands, w)
		case w == "--":
			ended = true
		case shortCluster.MatchString(w):
			for j := 1; j < len(w); j++ {
				short[w[j]] = true
			}
		case longFlag(w):
			long[w] = true
		default:
			operands = append(operands, w)
		}
	}
	for c := range f.short {
		if !short[c] {
			return false
		}
	}
	for w := range f.long {
		if !long[w] {
			return false
		}
	}
	return f.rest.MatchString(strings.Join(operands, " "))
}

// matchesFlagsAny reports a bash deny or ask rule whose flag form matches
// one of subjects.
func (r Rule) matchesFlagsAny(tool string, subjects []string) bool {
	if tool != "bash" || r.flags == nil || !r.ForTool(tool) {
		return false
	}
	for _, s := range subjects {
		if r.flags.matches(s) {
			return true
		}
	}
	return false
}
