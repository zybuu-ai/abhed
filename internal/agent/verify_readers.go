package agent

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The known readers: the few programs that, in the forms allowed here, change
// nothing. It is an allowlist kept small on purpose. A program is on it only
// when every option it is let through with is one that cannot write a file or
// run another program; anything else, including a long option that is not
// spelled out in full here (GNU and git programs take abbreviations), makes
// the command a possible change with unknown files. Being wrong this way
// leaves a claim unverified, which is always an acceptable answer.

// readerSpec says which options a reader may be given.
type readerSpec struct {
	// anyOption: the program has no option that writes or runs anything,
	// so every option, abbreviations included, is let through.
	anyOption bool
	// short are the short option letters allowed; shortValued those of them
	// that take a value, joined or as the next word.
	short, shortValued string
	// long are the long options allowed, spelled out in full; longValued
	// those that take a value as the next word when not given with =.
	long, longValued map[string]bool
	// maxOperands bounds the operands, -1 for any: a further one is a file
	// it writes, as uniq's second.
	maxOperands int
	// check, when set, judges the operands as well.
	check func(opts, ops []string) bool
}

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// parseReader reads a reader's words against its spec: the options it was
// given, its operands, and whether every option is allowed.
func parseReader(spec readerSpec, words []string) (opts, ops []string, ok bool) {
	for i := 1; i < len(words); i++ {
		a := words[i]
		switch {
		case a == "--":
			return opts, append(ops, words[i+1:]...), true
		case a == "-" || !strings.HasPrefix(a, "-"):
			ops = append(ops, a)
		case spec.anyOption:
			opts = append(opts, a)
		case strings.HasPrefix(a, "--"):
			name, _, eq := strings.Cut(a, "=")
			if !spec.long[name] {
				return nil, nil, false
			}
			opts = append(opts, a)
			if spec.longValued[name] && !eq {
				i++
			}
		default:
			opts = append(opts, a)
			for j := 1; j < len(a); j++ {
				c := a[j]
				if !strings.ContainsRune(spec.short, rune(c)) {
					return nil, nil, false
				}
				if strings.ContainsRune(spec.shortValued, rune(c)) {
					if j == len(a)-1 {
						i++ // the value is the next word
					}
					break
				}
			}
		}
	}
	return opts, ops, true
}

// pureReader reports whether a simple command, its words literal and its
// runners taken off, only reads.
func pureReader(words []string) bool {
	if len(words) == 0 {
		return true
	}
	prog := filepath.Base(words[0])
	switch prog {
	case "git":
		return gitReader(words)
	case "go":
		return goReader(words)
	case "find":
		return findReader(words)
	case "eslint", "mypy", "tsc", "staticcheck", "cargo":
		return lintReader(prog, words)
	}
	spec, ok := readerSpecs[prog]
	if !ok {
		return false
	}
	opts, ops, ok := parseReader(spec, words)
	if !ok || (spec.maxOperands >= 0 && len(ops) > spec.maxOperands) {
		return false
	}
	return spec.check == nil || spec.check(opts, ops)
}

// anything: a program with no option that writes a file or runs another.
var anything = readerSpec{anyOption: true, maxOperands: -1}

var readerSpecs = map[string]readerSpec{
	"cat": anything, "head": anything, "tail": anything, "wc": anything, "ls": anything, "stat": anything,
	"grep": anything, "egrep": anything, "fgrep": anything, "diff": anything, "cmp": anything, "jq": anything,
	"cut": anything, "tr": anything, "echo": anything, "pwd": anything, "true": anything, "false": anything,
	":": anything, "test": anything, "[": anything, "which": anything, "type": anything, "basename": anything,
	"dirname": anything, "realpath": anything, "readlink": anything,
	// ripgrep: --pre and --pre-glob run a program on each file, and -z runs
	// decompressors; it takes no abbreviations.
	"rg": {short: "iSFwxnNclLvoqsuUHIabkpe0fgtTmABCMjrE", shortValued: "efgtTmABCMjrE",
		long: set("--ignore-case", "--smart-case", "--case-sensitive", "--fixed-strings", "--word-regexp", "--line-regexp",
			"--line-number", "--no-line-number", "--count", "--count-matches", "--files", "--files-with-matches",
			"--files-without-match", "--glob", "--iglob", "--type", "--type-not", "--hidden", "--no-ignore", "--context",
			"--after-context", "--before-context", "--max-count", "--only-matching", "--json", "--color", "--heading",
			"--no-heading", "--multiline", "--invert-match", "--follow", "--max-depth", "--sort", "--sortr", "--vimgrep",
			"--replace", "--regexp", "--file", "--quiet", "--with-filename", "--no-filename", "--null", "--trim", "--unrestricted"),
		longValued: set("--glob", "--iglob", "--type", "--type-not", "--context", "--after-context", "--before-context",
			"--max-count", "--max-depth", "--sort", "--sortr", "--replace", "--regexp", "--file", "--color"),
		maxOperands: -1},
	// sed only as sed -n 'script' with print commands: see sedPrintOnly. -e,
	// -f, -i and the long options they abbreviate are not let through.
	"sed": {short: "nEruz", long: set("--quiet", "--silent", "--regexp-extended", "--null-data", "--posix", "--debug",
		"--sandbox"), maxOperands: -1, check: sedPrintOnly},
	// sort writes with -o and --output, and runs --compress-program.
	"sort": {short: "bdfgiMhnRrVcCszuk0123456789tST", shortValued: "ktST",
		long: set("--ignore-leading-blanks", "--dictionary-order", "--ignore-case", "--general-numeric-sort",
			"--ignore-nonprinting", "--month-sort", "--human-numeric-sort", "--numeric-sort", "--random-sort",
			"--reverse", "--version-sort", "--check", "--stable", "--unique", "--zero-terminated", "--key",
			"--field-separator", "--buffer-size", "--parallel"),
		longValued: set("--key", "--field-separator", "--buffer-size", "--parallel"), maxOperands: -1},
	// uniq writes its second operand.
	"uniq": {short: "cdDiuzfsw0123456789", shortValued: "fsw",
		long: set("--count", "--repeated", "--all-repeated", "--unique", "--ignore-case", "--skip-fields", "--skip-chars",
			"--check-chars", "--zero-terminated", "--group"),
		longValued: set("--skip-fields", "--skip-chars", "--check-chars"), maxOperands: 1},
}

// sedPrint is one sed command that only prints: an optional address and p,
// = or l.
var sedPrint = regexp.MustCompile(`^((\d+|\$|/[^/]*/)(,(\d+|\$|/[^/]*/))?)?\s*!?\s*[p=l]$`)

// sedPrintOnly lets sed through only as sed -n whose script only prints.
func sedPrintOnly(opts, ops []string) bool {
	quiet := false
	for _, o := range opts {
		quiet = quiet || o == "--quiet" || o == "--silent" || (o[1] != '-' && strings.ContainsRune(o, 'n'))
	}
	if !quiet || len(ops) == 0 {
		return false
	}
	for _, part := range strings.FieldsFunc(ops[0], func(r rune) bool { return r == ';' || r == '\n' }) {
		if !sedPrint.MatchString(strings.TrimSpace(part)) {
			return false
		}
	}
	return true
}

// findReader lets find through when it neither deletes, runs nor writes.
func findReader(words []string) bool {
	for _, a := range words[1:] {
		switch a {
		case "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls":
			return false
		}
	}
	return true
}

// gitReads are git's reading subcommands and the long options each is let
// through with, in full: git takes abbreviations, and -O, -o and --output
// open or write files, while --ext-diff and --textconv run programs.
var gitReads = map[string]readerSpec{
	"status": {short: "sbuvz", long: set("--short", "--branch", "--porcelain", "--long", "--untracked-files", "--ignored",
		"--ignore-submodules", "--show-stash", "--ahead-behind", "--no-ahead-behind", "--verbose"), maxOperands: -1},
	"diff": {short: "pusRaUMCBWz0123456789", shortValued: "", long: set("--stat", "--numstat", "--shortstat", "--name-only",
		"--name-status", "--cached", "--staged", "--color", "--no-color", "--word-diff", "--unified", "--patch", "--no-patch",
		"--minimal", "--ignore-all-space", "--ignore-space-change", "--exit-code", "--quiet", "--summary", "--diff-filter",
		"--relative", "--no-renames", "--check", "--raw", "--no-ext-diff", "--no-textconv"), maxOperands: -1},
	"log": {short: "pnSGL0123456789", shortValued: "nSGL", long: set("--oneline", "--graph", "--decorate", "--all", "--stat",
		"--patch", "--name-only", "--name-status", "--format", "--pretty", "--since", "--until", "--after", "--before",
		"--author", "--grep", "--max-count", "--follow", "--reverse", "--no-merges", "--merges", "--abbrev-commit", "--date",
		"--first-parent", "--no-ext-diff", "--no-textconv", "--no-color", "--color"),
		longValued: set("--author", "--grep", "--max-count", "--since", "--until", "--after", "--before"), maxOperands: -1},
	"show": {short: "sq", long: set("--stat", "--name-only", "--name-status", "--format", "--pretty", "--patch", "--no-patch",
		"--oneline", "--color", "--no-color", "--quiet", "--no-ext-diff", "--no-textconv"), maxOperands: -1},
	"rev-parse": {short: "q", long: set("--show-toplevel", "--abbrev-ref", "--short", "--verify", "--git-dir",
		"--is-inside-work-tree", "--show-prefix", "--symbolic-full-name", "--quiet", "--show-cdup"), maxOperands: -1},
}

// gitReader lets git through for its reading subcommands, with no global
// option before them (-C, -c and the rest change what it does).
func gitReader(words []string) bool {
	if len(words) < 2 {
		return false
	}
	spec, ok := gitReads[words[1]]
	if !ok {
		return false
	}
	_, _, ok = parseReader(spec, words[1:])
	return ok
}

// goTestFlags are the go test and go vet flags allowed, as go spells them (one
// dash or two, a value with = or as the next word): none writes a file or
// runs a program. -o, -c, -exec, -vettool, the profiles, -fuzz, -args and the
// rest are not here.
var goTestFlags = map[string]bool{
	"run": true, "v": false, "count": true, "race": false, "short": false, "timeout": true, "cover": false,
	"covermode": true, "coverpkg": true, "failfast": false, "bench": true, "benchmem": false, "benchtime": true,
	"cpu": true, "p": true, "parallel": true, "tags": true, "json": false, "list": true, "shuffle": true, "skip": true,
	"vet": true, "mod": true, "trimpath": false,
}

// goReader lets go test and go vet through with only the flags above.
func goReader(words []string) bool {
	if len(words) < 2 || (words[1] != "test" && words[1] != "vet") {
		return false
	}
	for i := 2; i < len(words); i++ {
		a := words[i]
		if !strings.HasPrefix(a, "-") {
			continue // a package pattern
		}
		name, _, eq := strings.Cut(strings.TrimLeft(a, "-"), "=")
		valued, ok := goTestFlags[name]
		if !ok {
			return false
		}
		if valued && !eq {
			i++
		}
	}
	return true
}

// lintReader: a lint run that reports and does not fix (no --fix, no output
// file), by a tool whose config cannot turn fixing on.
func lintReader(prog string, words []string) bool {
	sub := ""
	if len(words) > 1 {
		sub = words[1]
	}
	switch {
	case prog == "cargo" && sub != "clippy":
		return false
	case prog == "tsc" && !slices.Contains(words, "--noEmit"):
		return false
	}
	for _, a := range words[1:] {
		if a == "--" && prog == "cargo" {
			break // clippy's own lint flags follow
		}
		name, _, _ := strings.Cut(a, "=")
		switch {
		case strings.HasPrefix(name, "--fix") && name != "--fix-dry-run", name == "--unsafe-fixes", name == "--add-noqa",
			name == "-o", strings.Contains(name, "output"), strings.HasPrefix(name, "--out"), strings.HasPrefix(name, "--cache"),
			name == "--install-types", name == "-b", name == "--build", name == "-w", name == "--watch", name == "--":
			return false
		}
	}
	return true
}
