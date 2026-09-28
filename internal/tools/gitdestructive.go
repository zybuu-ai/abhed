package tools

import (
	"path"
	"runtime"
	"strings"
	"sync/atomic"
)

// foldCommandNames is set where the filesystem ignores case, so GIT and Rm
// run git and rm, and a program name is compared without case.
var foldCommandNames atomic.Bool

func init() { foldCommandNames.Store(runtime.GOOS == "darwin" || runtime.GOOS == "windows") }

// FoldsCommandNames reports whether program names are compared without case here.
func FoldsCommandNames() bool { return foldCommandNames.Load() }

// FoldCommandNamesForTest sets folding for a test and returns the undo.
func FoldCommandNamesForTest(on bool) (restore func()) {
	was := foldCommandNames.Swap(on)
	return func() { foldCommandNames.Store(was) }
}

// CommandName is a program name as it is compared: lowered where case is ignored.
func CommandName(w string) string {
	if FoldsCommandNames() {
		return strings.ToLower(w)
	}
	return w
}

// foldProgramNames lowers the words the shell runs as programs, where case is
// ignored, and leaves every argument as written.
func foldProgramNames(command string) string {
	if !FoldsCommandNames() {
		return command
	}
	const breaks = ";&|()`\n\r"
	var b strings.Builder
	pos := position{on: true}
	for i := 0; i < len(command); {
		c := command[i]
		switch {
		case strings.IndexByte(breaks, c) >= 0:
			pos = position{on: true}
			b.WriteByte(c)
			i++
			continue
		case c == ' ' || c == '\t':
			b.WriteByte(c)
			i++
			continue
		}
		j := i
		for j < len(command) && strings.IndexByte(breaks+" \t", command[j]) < 0 {
			j++
		}
		w := command[i:j]
		if pos.next(w) && !strings.HasPrefix(w, "-") && !strings.Contains(w, "=") {
			w = strings.ToLower(w)
		}
		b.WriteString(w)
		i = j
	}
	return b.String()
}

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
// The line is read again with each command substitution as one word, so a
// program name such as $(which git) is read with the words after it.
func gitDestructive(command string) (string, bool) {
	for _, line := range []string{command, collapseSubstitutions(command)} {
		if what, ok := gitDestructiveParts(line); ok {
			return what, true
		}
	}
	return "", false
}

func gitDestructiveParts(command string) (string, bool) {
	for _, part := range strings.Split(shellBreaks.Replace(command), "\n") {
		words := strings.Fields(shellQuotes.Replace(part))
		seen := 0
		pos := position{on: true}
		for i, w := range words {
			run := pos.next(w)
			// A substitution that names git is read as git only where it is the program.
			named := strings.TrimSuffix(CommandName(path.Base(w)), ".exe") == "git" || (run && w == gitSubstitution)
			if !named {
				continue
			}
			if seen++; seen > maxGitWords {
				return "too many git commands in one line to check", true
			}
			if what, ok := gitDiscards(words[i+1:], run); ok {
				return what, true
			}
		}
	}
	return "", false
}

// The words a command substitution is read as, the second when it names git.
const (
	substitution    = "$SUBST"
	gitSubstitution = "$GITSUBST"
)

// collapseSubstitutions replaces each $(...) and `...` with one word, so the
// words after it stay in its command.
func collapseSubstitutions(command string) string {
	var b strings.Builder
	for i := 0; i < len(command); i++ {
		switch {
		case command[i] == '$' && i+1 < len(command) && command[i+1] == '(':
			depth, j := 0, i+1
			for ; j < len(command); j++ {
				if command[j] == '(' {
					depth++
				} else if command[j] == ')' {
					if depth--; depth == 0 {
						break
					}
				}
			}
			b.WriteString(substitutionWord(command[i+2 : min(j, len(command))]))
			i = j
		case command[i] == '`':
			j := strings.IndexByte(command[i+1:], '`')
			if j < 0 {
				j = len(command) - i - 1
			}
			b.WriteString(substitutionWord(command[i+1 : i+1+j]))
			i += j + 1
		default:
			b.WriteByte(command[i])
		}
	}
	return b.String()
}

func substitutionWord(inner string) string {
	if strings.Contains(CommandName(inner), "git") {
		return gitSubstitution
	}
	return substitution
}

// gitRunners run the command after their options, so a git after them is the
// command; a shell's -c text loses its quotes here and reads the same way.
var gitRunners = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "env": true, "command": true, "exec": true, "nohup": true, "nice": true, "sudo": true, "doas": true,
	"time": true, "timeout": true, "xargs": true, "setsid": true, "stdbuf": true, "ionice": true, "builtin": true,
}

// position follows a simple command's words to the program it runs: past
// assignments, runners, shell keywords, options and an option's value.
type position struct{ on, afterOption bool }

// next reports whether w is at the program's position, then moves past it.
func (p *position) next(w string) bool {
	was := p.on
	switch {
	case !p.on:
	case strings.HasPrefix(w, "-"):
		p.afterOption = true
		return was
	case strings.Contains(w, "=") || gitRunners[CommandName(path.Base(w))] || shellKeywords[w] || isDuration(w):
	case p.afterOption:
	default:
		p.on = false
	}
	p.afterOption = false
	return was
}

// shellKeywords come before a command without being it.
var shellKeywords = map[string]bool{
	"!": true, "{": true, "if": true, "then": true, "else": true, "elif": true, "do": true,
	"while": true, "until": true, "eval": true,
}

func isDuration(w string) bool {
	return w != "" && strings.Trim(w, "0123456789.smhd") == ""
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

// gitDiscards reads the words after git. run is true when that git is the
// program the shell runs, so a subcommand git does not have is an alias or an
// extension whose effect cannot be read here.
func gitDiscards(words []string, run bool) (string, bool) {
	i := 0
	for i < len(words) && strings.HasPrefix(words[i], "-") {
		if value, ok := gitGlobalValue(words, i); ok && configAliases(words[i], value) {
			return "a git alias set on the command line, which may run anything", true
		}
		if gitGlobalsWithValue[words[i]] {
			i++
		}
		i++
	}
	if i >= len(words) {
		return "", false
	}
	sub := words[i]
	if run && !gitCommands[sub] {
		return "git " + sub + ", an alias or extension that cannot be checked", true
	}
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
	case "read-tree":
		if g.risky("", "u") {
			return "overwrite the working tree (git read-tree -u)", true
		}
	case "checkout-index":
		if g.risky("force", "f") {
			return "overwrite working-tree files (git checkout-index -f)", true
		}
	case "update-ref":
		return "delete or move a ref (git update-ref)", true
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

// gitGlobalValue is the value of the global option at words[i], attached or next.
func gitGlobalValue(words []string, i int) (string, bool) {
	if name, value, ok := strings.Cut(words[i], "="); ok && strings.HasPrefix(name, "--") {
		return value, true
	}
	if gitGlobalsWithValue[words[i]] && i+1 < len(words) {
		return words[i+1], true
	}
	return "", false
}

// configAliases reports a -c or --config-env setting that can define an alias:
// an alias itself, or an include that reads another file's.
func configAliases(option, value string) bool {
	if name, _, _ := strings.Cut(option, "="); name != "-c" && name != "--config-env" {
		return false
	}
	key := strings.ToLower(value)
	return strings.HasPrefix(key, "alias.") || strings.HasPrefix(key, "include.") || strings.HasPrefix(key, "includeif.")
}

// gitCommands are git's own subcommands, which no alias can replace: those
// `git --list-cmds=builtins,main,others` lists with git 2.39, and later ones.
var gitCommands = set(
	"add", "add--interactive", "am", "annotate", "apply", "archimport", "archive", "backfill",
	"bisect", "bisect--helper", "blame", "branch", "bugreport", "bundle", "cat-file", "check-attr",
	"check-ignore", "check-mailmap", "check-ref-format", "checkout", "checkout--worker",
	"checkout-index", "cherry", "cherry-pick", "citool", "clean", "clone", "column", "commit",
	"commit-graph", "commit-tree", "config", "count-objects", "credential", "credential-cache",
	"credential-cache--daemon", "credential-osxkeychain", "credential-store", "cvsexportcommit",
	"cvsimport", "cvsserver", "daemon", "describe", "diagnose", "diff", "diff-files", "diff-index",
	"diff-tree", "difftool", "difftool--helper", "env--helper", "fast-export", "fast-import", "fetch",
	"fetch-pack", "filter-branch", "fmt-merge-msg", "for-each-ref", "for-each-repo", "format-patch",
	"fsck", "fsck-objects", "fsmonitor--daemon", "gc", "get-tar-commit-id", "grep", "gui",
	"gui--askpass", "hash-object", "help", "hook", "http-backend", "http-fetch", "http-push",
	"imap-send", "index-pack", "init", "init-db", "instaweb", "interpret-trailers", "last-modified",
	"log", "ls-files", "ls-remote", "ls-tree", "mailinfo", "mailsplit", "maintenance", "merge",
	"merge-base", "merge-file", "merge-index", "merge-octopus", "merge-one-file", "merge-ours",
	"merge-recursive", "merge-recursive-ours", "merge-recursive-theirs", "merge-resolve",
	"merge-subtree", "merge-tree", "mergetool", "mktag", "mktree", "multi-pack-index", "mv",
	"name-rev", "notes", "p4", "pack-objects", "pack-redundant", "pack-refs", "patch-id", "pickaxe",
	"prune", "prune-packed", "pull", "push", "quiltimport", "range-diff", "read-tree", "rebase",
	"receive-pack", "reflog", "refs", "remote", "remote-ext", "remote-fd", "remote-ftp",
	"remote-ftps", "remote-http", "remote-https", "repack", "replace", "replay", "repo",
	"request-pull", "rerere", "reset", "restore", "rev-list", "rev-parse", "revert", "rm", "scalar",
	"send-email", "send-pack", "sh-i18n--envsubst", "shell", "shortlog", "show", "show-branch",
	"show-index", "show-ref", "sparse-checkout", "stage", "stash", "status", "stripspace",
	"submodule", "submodule--helper", "subtree", "svn", "switch", "symbolic-ref", "tag",
	"unpack-file", "unpack-objects", "update-index", "update-ref", "update-server-info",
	"upload-archive", "upload-archive--writer", "upload-pack", "var", "verify-commit", "verify-pack",
	"verify-tag", "version", "web--browse", "whatchanged", "worktree", "write-tree",
)

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}
