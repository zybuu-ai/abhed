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
	return len(w) > 2 && strings.HasPrefix(w, "--") && !strings.ContainsAny(w, "=*?")
}

// longFlagStar is a long flag a rule's glob ends with a star: --force*.
var longFlagStar = regexp.MustCompile(`^--[A-Za-z0-9][A-Za-z0-9-]*\*$`)

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
		case longFlagStar.MatchString(w):
			// --force* in bash(git push --force*): the flag, then anything.
			f.long[w[:len(w)-1]] = true
			fields[i] = "*"
			break flags
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
			have[canonFlag(eq, expandLong(eq, f.long, w))] = true
		case strings.HasPrefix(w, "--") && strings.Contains(w, "=") && eq[expandLong(eq, f.long, w[:strings.IndexByte(w, '=')])] != "":
			// --force-with-lease=main is the flag its table names.
			have[canonFlag(eq, expandLong(eq, f.long, w[:strings.IndexByte(w, '=')]))] = true
		case len(w) > 1 && w[0] == '+' && eq[forceRefspec] != "":
			// git push origin +main forces that ref, as -f does.
			have[eq[forceRefspec]] = true
			operands = append(operands, w)
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
	"git push":  {"--force": "-f", "--force-with-lease": "-f", forceRefspec: "-f"},
	"git clean": {"--force": "-f"},
}

// expandLong is w, a long option as a command wrote it, as the long flag it
// abbreviates, the way GNU getopt and git read --recur for --recursive: a
// prefix, of any length as getopt takes it, of the table's flags or the rule's own,
// whose candidates all mean the same. w itself where none or several differ.
func expandLong(eq map[string]string, rule map[string]bool, w string) string {
	if _, known := eq[w]; known || rule[w] || len(w) < len("--")+1 {
		return w
	}
	found, canon := "", ""
	consider := func(full string) bool {
		if !strings.HasPrefix(full, w) {
			return true
		}
		c := canonFlag(eq, full)
		if found != "" && c != canon {
			return false // ambiguous between flags that differ
		}
		if found == "" {
			found, canon = full, c
		}
		return true
	}
	for full := range eq {
		if strings.HasPrefix(full, "--") && !consider(full) {
			return w
		}
	}
	for full := range rule {
		if !consider(full) {
			return w
		}
	}
	if found == "" {
		return w
	}
	return found
}

// forceRefspec stands in the table for a refspec that forces (+main).
const forceRefspec = "+refspec"

// canonFlag is flag as its command's synonym table spells it.
func canonFlag(eq map[string]string, flag string) string {
	if c, ok := eq[flag]; ok {
		return c
	}
	return flag
}

// gitGlobalWithValue are git's options before the subcommand that take the
// next word as their value when it is not glued on with =.
var gitGlobalWithValue = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--config-env": true, "--exec-path": true, "--attr-source": true, "--super-prefix": true}

// skipGitGlobals drops git's options between git and its subcommand, so
// git -C dir --no-pager push -f is read as git push -f: every word starting
// with - goes, and one of gitGlobalWithValue takes the next word with it.
// For deny and ask matching only.
func skipGitGlobals(fields []string) []string {
	if len(fields) == 0 || tools.CommandName(path.Base(fields[0])) != "git" {
		return fields
	}
	i := 1
	for i < len(fields) && strings.HasPrefix(fields[i], "-") {
		if gitGlobalWithValue[fields[i]] {
			i++
		}
		i++
	}
	if i > len(fields) {
		i = len(fields)
	}
	return append([]string{fields[0]}, fields[i:]...)
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
