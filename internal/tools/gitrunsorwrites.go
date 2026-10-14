package tools

import (
	"path"
	"strings"
)

// gitRunsOrWritesLong are git options, after the subcommand, that write a file
// or run a program, with the shortest prefix of each read as it: a git that
// accepts abbreviations runs --ext as --ext-diff.
var gitRunsOrWritesLong = []struct {
	name string
	min  int
	why  string
}{
	{"output", 3, "writes its output to the file it names"},
	{"output-directory", 3, "writes patch files into the folder it names"},
	{"ext-diff", 3, "runs the external diff program git's configuration names"},
	// --text is its own option, so only a longer prefix is --textconv.
	{"textconv", 5, "runs the text conversion programs git's configuration names"},
	{"open-files-in-pager", 3, "runs a pager program on the matching files"},
	{"upload-pack", 3, "runs the program it names"},
	{"receive-pack", 3, "runs the program it names"},
	{"exec", 3, "runs the command it names"},
}

// gitRunsOrWritesShort are the short forms of those options, per subcommand.
var gitRunsOrWritesShort = map[string]struct{ letters, why string }{
	"grep":         {"O", "runs a pager program on the matching files"},
	"format-patch": {"o", "writes patch files into the folder it names"},
	"clone":        {"u", "runs the program it names"},
	"ls-remote":    {"u", "runs the program it names"},
	"rebase":       {"x", "runs the command it names"},
}

// GitRunsOrWrites says why the command runs git with an option that writes a
// file or runs a program, such as --output, --ext-diff or -c before the
// subcommand; "" when none. A read-only allow rule such as bash(git diff *)
// does not approve these. Rough on purpose: a false match only asks.
func (c Canonical) GitRunsOrWrites() string {
	forms := append([]string{c.command, c.Text}, c.shell.commands...)
	for _, form := range forms {
		for _, line := range []string{form, collapseSubstitutions(form)} {
			if why := gitRunsOrWritesParts(line); why != "" {
				return why
			}
		}
	}
	return ""
}

func gitRunsOrWritesParts(command string) string {
	for _, part := range strings.Split(shellBreaks.Replace(command), "\n") {
		words := strings.Fields(shellQuotes.Replace(part))
		seen := 0
		pos := position{on: true}
		for i, w := range words {
			run := pos.next(w)
			if strings.TrimSuffix(CommandName(path.Base(w)), ".exe") != "git" && (!run || w != gitSubstitution) {
				continue
			}
			if seen++; seen > maxGitWords {
				return "too many git commands in one line to check"
			}
			if why := gitOptionRunsOrWrites(words[i+1:]); why != "" {
				return why
			}
		}
	}
	return ""
}

// gitOptionRunsOrWrites reads the words after git: its options before the
// subcommand, then every option of the subcommand before --, wherever it
// sits among the operands, as git reads them.
func gitOptionRunsOrWrites(words []string) string {
	i := 0
	for ; i < len(words) && strings.HasPrefix(words[i], "-"); i++ {
		w := words[i]
		switch {
		case strings.HasPrefix(w, "-c"), strings.HasPrefix(w, "--config-env"):
			return "git " + w + " sets configuration, which can name a program to run"
		case strings.HasPrefix(w, "--exec-path"):
			return "git " + w + " sets where git finds its commands"
		}
		if gitGlobalsWithValue[w] {
			i++
		}
	}
	if i >= len(words) {
		return ""
	}
	sub := words[i]
	short := gitRunsOrWritesShort[sub]
	for _, w := range words[i+1:] {
		if w == "--" {
			break
		}
		if name, ok := strings.CutPrefix(w, "--"); ok {
			name, _, _ = strings.Cut(name, "=")
			for _, o := range gitRunsOrWritesLong {
				if len(name) >= min(o.min, len(o.name)) && strings.HasPrefix(o.name, name) {
					return "git " + sub + " --" + o.name + " " + o.why
				}
			}
			continue
		}
		// A short option anywhere in a cluster, value or not: -nOvim is -n -O vim.
		if short.letters != "" && len(w) > 1 && w[0] == '-' && strings.ContainsAny(w[1:], short.letters) {
			return "git " + sub + " " + w + " " + short.why
		}
	}
	return ""
}
