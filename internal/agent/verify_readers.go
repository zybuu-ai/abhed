package agent

import (
	"path/filepath"
	"regexp"
	"strings"
)

// The known readers: programs that, in the forms allowed here, change no file.
// It is an allowlist, like the ssh read commands: a program not here, or one
// here given an option or an operand that makes it write, counts as a change.

// pureReader reports whether a simple command, its words literal and its
// runners taken off, only reads.
func pureReader(words []string) bool {
	if len(words) == 0 {
		return true
	}
	prog := filepath.Base(words[0])
	if check, ok := readerPrograms[prog]; ok {
		return check(words)
	}
	return false
}

// none lets a reader through whatever its options.
func none(words []string) bool { return true }

// without lets a reader through unless it is given one of the options:
// short letters (alone or clustered) and long names, with or without =value.
func without(short string, long ...string) func([]string) bool {
	return func(words []string) bool {
		for _, a := range words[1:] {
			if a == "--" {
				break
			}
			for _, l := range long {
				if a == l || strings.HasPrefix(a, l+"=") {
					return false
				}
			}
			if short != "" && len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a[1:], short) {
				return false
			}
		}
		return true
	}
}

// atMost lets a reader through with no more than n operands: a further one
// is a file it writes, as uniq's or xxd's second.
func atMost(n int, also func([]string) bool) func([]string) bool {
	return func(words []string) bool {
		return len(operands(words)) <= n && (also == nil || also(words))
	}
}

// subcommands lets a program through for the listed subcommands, each with
// its own check.
func subcommands(subs map[string]func([]string) bool) func([]string) bool {
	return func(words []string) bool {
		if len(words) < 2 {
			return false
		}
		check, ok := subs[words[1]]
		return ok && check(words[1:])
	}
}

// fixers are the options that make a checker, linter or test runner change
// files: fix in place, write, or update snapshots and goldens.
var fixers = []string{"--fix", "--fix-only", "--write", "--update", "--update-snapshots", "--updateSnapshot", "--snapshot-update",
	"--allow-dirty", "--in-place", "--inplace", "--output", "--outfile", "--out-file", "-update", "-fix", "-w", "-u", "-o"}

// noFixers lets a checker or test runner through unless any word asks it to
// fix, write or update: the options above, in its own words or after "--".
func noFixers(words []string) bool {
	for _, a := range words[1:] {
		for _, f := range fixers {
			if a == f || strings.HasPrefix(a, f+"=") {
				return false
			}
		}
		// go test's -coverprofile and friends name a file it writes.
		if strings.HasSuffix(strings.SplitN(a, "=", 2)[0], "profile") || strings.HasPrefix(a, "-o=") {
			return false
		}
	}
	return true
}

var (
	// sedPrint is a sed script that only prints or substitutes into its output.
	sedPrint = regexp.MustCompile(`^(?:[0-9$,/^.*\[\]\\a-zA-Z_ -]*[pdq=]|s(.).*)$`)
)

// sedReader lets sed through when it is not in place and its script cannot
// write: no w, W, r, R or e command or flag, no script file.
func sedReader(words []string) bool {
	if !without("if", "--in-place", "--file")(words) {
		return false
	}
	scripts := []string{}
	expr := false
	for i := 1; i < len(words); i++ {
		a := words[i]
		switch {
		case a == "-e" || a == "--expression":
			if i+1 < len(words) {
				scripts = append(scripts, words[i+1])
				i++
			}
			expr = true
		case strings.HasPrefix(a, "--expression="):
			scripts, expr = append(scripts, strings.TrimPrefix(a, "--expression=")), true
		case strings.HasPrefix(a, "-e") && len(a) > 2:
			scripts, expr = append(scripts, a[2:]), true
		}
	}
	if !expr {
		ops := operands(words)
		if len(ops) == 0 {
			return false
		}
		scripts = ops[:1]
	}
	for _, sc := range scripts {
		for _, part := range strings.FieldsFunc(sc, func(r rune) bool { return r == ';' || r == '\n' }) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !sedPrint.MatchString(part) || sedWrites(part) {
				return false
			}
		}
	}
	return true
}

// sedWrites reports a sed command that writes or runs: w, W, r, R, e, or an
// s command whose flags hold w or e.
func sedWrites(part string) bool {
	if part[0] == 's' && len(part) > 1 {
		d := part[1]
		if i := strings.LastIndexByte(part, d); i > 1 {
			return strings.ContainsAny(part[i+1:], "weWE")
		}
		return true
	}
	return strings.ContainsAny(part, "wWrRe")
}

// awkReader lets awk through when its program cannot write a file or run a
// command and it is not editing in place.
func awkReader(words []string) bool {
	for i, a := range words[1:] {
		if a == "-i" || a == "--include" || strings.HasPrefix(a, "-i") || a == "-f" || strings.HasPrefix(a, "--file") {
			return false
		}
		_ = i
		if strings.ContainsAny(a, ">|") || strings.Contains(a, "system") || strings.Contains(a, "getline") {
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

// gitReader lets git through for the subcommands that leave the working
// tree, the index and the refs alone, in their listing forms.
func gitReader(words []string) bool {
	if len(words) < 2 || strings.HasPrefix(words[1], "-") {
		return false // global options, such as -C, change what it reads
	}
	sub, rest := words[1], words[2:]
	switch sub {
	case "status", "rev-parse", "blame", "ls-files", "ls-tree", "describe", "shortlog", "grep", "cat-file", "rev-list",
		"show-ref", "merge-base", "name-rev", "for-each-ref", "help", "version", "count-objects", "check-ignore":
		return true
	case "diff", "log", "show":
		return without("", "--output", "--ext-diff")(words[1:])
	case "branch":
		return len(rest) == 0 || allIn(rest, "-a", "-r", "-v", "-vv", "--list", "-l", "--show-current", "--all", "--remotes")
	case "tag":
		return len(rest) == 0 || allIn(rest, "-l", "--list")
	case "remote":
		return len(rest) == 0 || allIn(rest, "-v") || (rest[0] == "get-url" || rest[0] == "show")
	case "config":
		return len(rest) > 0 && (rest[0] == "--get" || rest[0] == "--get-all" || rest[0] == "--list" || rest[0] == "-l" || rest[0] == "--get-regexp")
	case "stash":
		return len(rest) > 0 && (rest[0] == "list" || rest[0] == "show")
	}
	return false
}

func allIn(words []string, allowed ...string) bool {
	for _, w := range words {
		ok := false
		for _, a := range allowed {
			ok = ok || w == a
		}
		if !ok {
			return false
		}
	}
	return true
}

// goReader lets go through for test, vet, build, list, version, env and doc,
// without the options that write files (-o, a profile, -update) or settings.
func goReader(words []string) bool {
	if len(words) < 2 {
		return false
	}
	switch words[1] {
	case "test", "vet", "build", "list", "doc", "version":
		return noFixers(words[1:])
	case "env":
		return without("wu")(words[1:])
	}
	return false
}

// scriptReader lets npm, pnpm, yarn and bun through for a test, lint, check
// or build script, with nothing after it that fixes, writes or updates.
func scriptReader(words []string) bool {
	if len(words) < 2 {
		return false
	}
	script := words[1]
	if script == "run" || script == "run-script" {
		if len(words) < 3 {
			return false
		}
		script = words[2]
	}
	if script == "test" || script == "t" || strings.HasPrefix(script, "test:") || script == "lint" || strings.HasPrefix(script, "lint:") ||
		script == "check" || script == "typecheck" || script == "build" || strings.HasPrefix(script, "build:") {
		return noFixers(words)
	}
	// yarn jest and the like: the binary it runs, judged as itself.
	if filepath.Base(words[0]) != "npm" && words[1] != "run" {
		return pureReader(words[1:])
	}
	return false
}

// makeReader lets make through for its checking targets only: make and make
// all build, which writes.
func makeReader(words []string) bool {
	if len(words) < 2 {
		return false
	}
	return allIn(words[1:], "test", "check", "lint", "vet", "tests", "-k", "-s")
}

// tscReader lets tsc through only when it emits nothing.
func tscReader(words []string) bool {
	return noFixers(words) && (allIn([]string{"--noEmit"}, words[1:]...) || hasWord(words, "--noEmit"))
}

func hasWord(words []string, w string) bool {
	for _, a := range words {
		if a == w {
			return true
		}
	}
	return false
}

// kubectlReader lets kubectl through for the commands that only read.
func kubectlReader(words []string) bool {
	for _, a := range words[1:] {
		if strings.HasPrefix(a, "-") {
			continue
		}
		switch a {
		case "get", "describe", "logs", "top", "explain", "version", "api-resources", "api-versions", "cluster-info":
			return true
		case "config":
			return hasWord(words, "view") || hasWord(words, "get-contexts") || hasWord(words, "current-context")
		}
		return false
	}
	return false
}

// curlReader lets curl through when it saves nothing and sends nothing.
func curlReader(words []string) bool {
	return without("oOTdFX", "--output", "--remote-name", "--upload-file", "--data", "--data-binary", "--data-raw", "--form",
		"--request", "--remote-name-all", "--output-dir", "--create-dirs", "--cookie-jar", "--dump-header", "--trace", "--trace-ascii", "--stderr")(words)
}

var readerPrograms map[string]func([]string) bool

func init() {
	readerPrograms = map[string]func([]string) bool{
		"ls": none, "cat": none, "head": none, "tail": none, "wc": none, "pwd": none, "echo": none, "printf": without("v"),
		"true": none, "false": none, ":": none, "which": none, "type": none, "whoami": none, "id": none, "uname": none,
		"stat": none, "file": without("C", "--compile"), "du": none, "df": none, "basename": none, "dirname": none,
		"realpath": none, "readlink": none, "nl": none, "column": none, "cut": none, "tr": none, "comm": none, "cmp": none,
		"diff": without("", "--output"), "md5sum": none, "md5": none, "sha256sum": none, "sha1sum": none, "shasum": none,
		"hexdump": none, "od": none, "strings": none, "less": none, "more": none, "ps": none, "uptime": none, "free": none,
		"nproc": none, "lsof": none, "printenv": none, "sleep": none, "test": none, "[": none, "env": atMost(0, nil),
		"date": without("s", "--set"), "hostname": atMost(0, nil), "tree": without("o", "--output"),
		"sort": without("o", "--output"), "uniq": atMost(1, nil), "xxd": atMost(1, without("r", "--revert")),
		"grep": without("", "--pre"), "egrep": none, "fgrep": none, "rg": without("", "--pre"),
		"jq": none, "yq": without("i", "--inplace"), "sed": sedReader, "gsed": sedReader, "awk": awkReader, "gawk": awkReader,
		"find": findReader, "git": gitReader, "go": goReader, "make": makeReader, "kubectl": kubectlReader, "curl": curlReader,
		"cargo": subcommands(map[string]func([]string) bool{"test": noFixers, "check": noFixers, "build": noFixers, "clippy": noFixers}),
		"npm":   scriptReader, "pnpm": scriptReader, "yarn": scriptReader, "bun": scriptReader,
		"pytest": noFixers, "jest": noFixers, "vitest": noFixers, "mocha": noFixers, "rspec": noFixers, "phpunit": noFixers,
		"mypy": noFixers, "flake8": noFixers, "pylint": noFixers, "shellcheck": noFixers, "eslint": noFixers,
		"golangci-lint": noFixers, "tsc": tscReader,
		"ruff":   subcommands(map[string]func([]string) bool{"check": noFixers}),
		"python": pythonReader, "python3": pythonReader,
	}
}

// pythonReader lets python through only to run a test runner or checker
// module, judged as that module.
func pythonReader(words []string) bool {
	if len(words) < 3 || words[1] != "-m" {
		return false
	}
	switch words[2] {
	case "pytest", "unittest", "mypy", "pylint", "flake8":
		return noFixers(words[2:])
	}
	return false
}
