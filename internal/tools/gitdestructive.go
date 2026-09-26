package tools

import (
	"path"
	"strings"
)

// Split points between the simple commands of a line, and the quoting a
// word loses on its way to git. Rough on purpose: a false match only asks.
var (
	shellBreaks = strings.NewReplacer(";", "\n", "&", "\n", "|", "\n", "(", "\n", ")", "\n", "`", "\n")
	shellQuotes = strings.NewReplacer("$'", "", `$"`, "", "'", "", `"`, "", `\`, "")
)

// gitGlobalsWithValue are git's options before the subcommand that take the next word.
var gitGlobalsWithValue = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--config-env": true, "--super-prefix": true, "--attr-source": true,
}

// gitValues are, per subcommand, the short options that take a value (the
// rest of their cluster, or the next word) and the long ones that take the next word.
var gitValues = map[string]struct {
	short string
	long  []string
}{
	"restore":  {"s", []string{"source", "pathspec-from-file", "conflict"}},
	"checkout": {"bB", []string{"orphan", "conflict", "pathspec-from-file"}},
	"clean":    {"e", []string{"exclude"}},
	"tag":      {"mFu", []string{"message", "file", "local-user", "sort", "format", "contains", "no-contains", "points-at", "merged", "no-merged", "cleanup"}},
	"branch":   {"u", []string{"set-upstream-to", "sort", "format", "contains", "no-contains", "points-at", "merged", "no-merged"}},
	"stash":    {"m", []string{"message", "pathspec-from-file"}},
}

// maxGitWords bounds the git words read in one simple command, each to its
// end, so a line costs linear time; past it the command is taken as destructive.
const maxGitWords = 16

// gitDestructive reports a git command anywhere in the line that throws away
// work with no undo, for the forms it knows. Every word named git is read to
// the end of its command: git takes options among operands, one of which may
// be named git, and the first git may be something else, such as a user name.
func gitDestructive(command string) (string, bool) {
	for _, part := range strings.Split(shellBreaks.Replace(command), "\n") {
		words := strings.Fields(shellQuotes.Replace(part))
		seen := 0
		for i, w := range words {
			if path.Base(w) != "git" {
				continue
			}
			if seen++; seen > maxGitWords {
				return "too many git commands in one line to check", true
			}
			if what, ok := gitDiscards(words[i+1:]); ok {
				return what, true
			}
		}
	}
	return "", false
}

// gitArgs is a git subcommand's words: flags before `--`, the other words
// before it, and the pathspec after it. Option values are left out.
type gitArgs struct {
	flags, operands, paths []string
	shortValues            string
}

func parseGitArgs(sub string, words []string) gitArgs {
	vals := gitValues[sub]
	g := gitArgs{shortValues: vals.short}
	dashdash := false
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case dashdash:
			g.paths = append(g.paths, w)
		case w == "--":
			dashdash = true
		case strings.HasPrefix(w, "--"):
			g.flags = append(g.flags, w)
			if name := w[2:]; !strings.Contains(name, "=") && abbreviates(name, vals.long...) {
				i++
			}
		case strings.HasPrefix(w, "-") && w != "-":
			g.flags = append(g.flags, w)
			// A value-taking option last in its cluster takes the next word.
			if vals.short != "" && strings.IndexAny(w[1:], vals.short) == len(w)-2 {
				i++
			}
		default:
			g.operands = append(g.operands, w)
		}
	}
	return g
}

// abbreviates reports a long option name written in full or shortened, as git accepts.
func abbreviates(name string, longs ...string) bool {
	for _, l := range longs {
		if name != "" && strings.HasPrefix(l, name) {
			return true
		}
	}
	return false
}

// risky reports a flag that could discard work: the long name or any prefix
// of it, or a short letter read before a value-taking option.
func (g gitArgs) risky(long, short string) bool {
	for _, f := range g.flags {
		if name, ok := strings.CutPrefix(f, "--"); ok {
			name, _, _ = strings.Cut(name, "=")
			if long != "" && abbreviates(name, long) {
				return true
			}
			continue
		}
		if g.inCluster(f, short) {
			return true
		}
	}
	return false
}

// safe reports a flag that makes a command harmless, only as written in
// full, or as its short letter read before a value-taking option. A later
// --no- form, shortened or not, takes it back.
func (g gitArgs) safe(long, short string) bool {
	on := false
	for _, f := range g.flags {
		switch {
		case f == "--"+long || (!strings.HasPrefix(f, "--") && g.inCluster(f, short)):
			on = true
		case strings.HasPrefix(f, "--no-") && abbreviates(strings.SplitN(f[5:], "=", 2)[0], long):
			on = false
		}
	}
	return on
}

// inCluster reads a short cluster left to right, stopping at an option that
// takes the rest of it as a value.
func (g gitArgs) inCluster(f, letters string) bool {
	for _, c := range f[1:] {
		if strings.ContainsRune(letters, c) {
			return true
		}
		if strings.ContainsRune(g.shortValues, c) {
			return false
		}
	}
	return false
}

// pathLike reports a word git would read as a pathspec rather than a branch.
func pathLike(w string) bool {
	return w == "." || w == ".." || strings.HasPrefix(w, "./") || strings.HasPrefix(w, "../") ||
		strings.HasPrefix(w, ":") || strings.ContainsAny(w, "*?[")
}

func gitDiscards(words []string) (string, bool) {
	i := 0
	for i < len(words) && strings.HasPrefix(words[i], "-") {
		if gitGlobalsWithValue[words[i]] {
			i++
		}
		i++
	}
	if i >= len(words) {
		return "", false
	}
	sub := words[i]
	g := parseGitArgs(sub, words[i+1:])
	// Every subcommand that takes diff or log options, stash list included, writes --output.
	if g.risky("output", "") {
		return "write over a file (git --output)", true
	}
	switch sub {
	case "restore":
		// Only --staged alone leaves the working tree as it is.
		if !g.safe("staged", "S") || g.risky("worktree", "W") {
			return "discard uncommitted changes (git restore)", true
		}
	case "checkout":
		if g.risky("force", "f") {
			return "discard uncommitted changes (git checkout -f)", true
		}
		if len(g.paths) > 0 || g.risky("pathspec-from-file", "") || g.risky("ours", "") || g.risky("theirs", "") {
			return "discard uncommitted changes to the named paths (git checkout)", true
		}
		// -b, -B and --orphan take a branch name and a start point, not paths.
		if g.safe("orphan", "bB") {
			break
		}
		for _, w := range g.operands {
			if pathLike(w) {
				return "discard uncommitted changes to the named paths (git checkout)", true
			}
		}
		if len(g.operands) > 1 {
			return "discard uncommitted changes to the named paths (git checkout)", true
		}
	case "switch":
		if g.risky("discard-changes", "") || g.risky("force", "f") {
			return "discard uncommitted changes (git switch --discard-changes)", true
		}
	case "stash":
		if len(g.operands) > 0 && (g.operands[0] == "drop" || g.operands[0] == "clear") {
			return "delete stashed changes (git stash " + g.operands[0] + ")", true
		}
	case "branch":
		if g.risky("delete", "dD") || g.risky("force", "fMC") {
			return "delete, reset or overwrite a branch", true
		}
	case "tag":
		if g.risky("delete", "d") || g.risky("force", "f") {
			return "delete or replace a tag", true
		}
	case "worktree":
		if len(g.operands) > 0 && g.operands[0] == "remove" && g.risky("force", "f") {
			return "remove a worktree and its uncommitted changes", true
		}
	case "clean":
		if !g.safe("dry-run", "n") {
			return "delete untracked files (git clean)", true
		}
	case "reset":
		if g.risky("hard", "") {
			return "hard reset", true
		}
	case "push":
		if g.risky("force", "f") || g.risky("force-with-lease", "") || g.risky("delete", "d") || g.risky("mirror", "") {
			return "force push", true
		}
		for _, w := range g.operands {
			if strings.HasPrefix(w, "+") || strings.HasPrefix(w, ":") {
				return "force push", true
			}
		}
	}
	return "", false
}
