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

// shortClusterStar is a cluster a rule's glob ends with a star: -rf*.
var shortClusterStar = regexp.MustCompile(`^-[A-Za-z0-9]+\*$`)

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
		if strings.HasPrefix(w, "-") {
			break
		}
		if strings.ContainsAny(w, "*?") {
			return nil
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
		case shortClusterStar.MatchString(w):
			// -rf* in bash(rm -rf*): the flags, then anything; the star
			// stands for the rest, which may be more flags or operands.
			for j := 1; j < len(w)-1; j++ {
				f.short[w[j]] = true
			}
			fields[i] = "*"
			break flags
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
	fields := skipGitGlobals(strings.Fields(command))
	if len(fields) < len(f.prefix) || tools.CommandName(path.Base(fields[0])) != f.prefix[0] {
		return false
	}
	for k := 1; k < len(f.prefix); k++ {
		if fields[k] != f.prefix[k] {
			return false
		}
	}
	eq := flagSynonyms[strings.Join(f.prefix, " ")]
	have := map[string]bool{}
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
				have[canonFlag(eq, "-"+string(w[j]))] = true
			}
		case longFlag(w):
			have[canonFlag(eq, w)] = true
		case strings.HasPrefix(w, "--") && strings.Contains(w, "=") && eq[w[:strings.IndexByte(w, '=')]] != "":
			// --force-with-lease=main is the flag its table names.
			have[canonFlag(eq, w[:strings.IndexByte(w, '=')])] = true
		default:
			operands = append(operands, w)
		}
	}
	for c := range f.short {
		if !have[canonFlag(eq, "-"+string(c))] {
			return false
		}
	}
	for w := range f.long {
		if !have[canonFlag(eq, w)] {
			return false
		}
	}
	return f.rest.MatchString(strings.Join(operands, " "))
}

// flagSynonyms are, for common destructive commands, flags that do the same
// as another, each mapped to one spelling: a deny or ask rule naming one
// also holds for the others. Read for deny and ask matching only.
var flagSynonyms = map[string]map[string]string{
	"rm":        {"-R": "-r", "--recursive": "-r", "--force": "-f"},
	"chmod":     {"--recursive": "-R"},
	"chown":     {"--recursive": "-R"},
	"chgrp":     {"--recursive": "-R"},
	"git push":  {"--force": "-f", "--force-with-lease": "-f"},
	"git clean": {"--force": "-f"},
}

// canonFlag is flag as its command's synonym table spells it.
func canonFlag(eq map[string]string, flag string) string {
	if c, ok := eq[flag]; ok {
		return c
	}
	return flag
}

// gitGlobalWithValue are git's options before the subcommand that take the
// next word as their value when not glued to it with =.
var gitGlobalWithValue = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true}

// skipGitGlobals drops git's global options between git and its subcommand,
// so git -C dir push -f is read as git push -f.
func skipGitGlobals(fields []string) []string {
	if len(fields) == 0 || tools.CommandName(path.Base(fields[0])) != "git" {
		return fields
	}
	out := []string{fields[0]}
	i := 1
	for i < len(fields) {
		w := fields[i]
		switch {
		case gitGlobalWithValue[w]:
			i += 2
			continue
		case strings.HasPrefix(w, "--git-dir=") || strings.HasPrefix(w, "--work-tree=") || strings.HasPrefix(w, "--namespace=") ||
			strings.HasPrefix(w, "-C") && len(w) > 2 || strings.HasPrefix(w, "-c") && len(w) > 2 && strings.Contains(w, "="):
			i++
			continue
		case w == "--no-pager" || w == "-P" || w == "--paginate" || w == "-p" || w == "--bare" || w == "--no-replace-objects" || w == "--literal-pathspecs":
			i++
			continue
		}
		break
	}
	return append(out, fields[i:]...)
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
