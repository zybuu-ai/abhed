package agent

import (
	"path/filepath"
	"strings"
)

// Reading a shell command the way the checks need it: the simple commands
// it runs, which of them run tests or a build, and which files it writes.
// It errs toward "not a test run" and "not a write target", so a claim is
// left unverified rather than wrongly verified.

// shellWords splits one simple command into words, honouring single and
// double quotes and a backslash outside single quotes.
func shellWords(s string) []string {
	var out []string
	var b strings.Builder
	in, quoted := rune(0), false
	for _, r := range s {
		switch {
		case in == '\'':
			if r == '\'' {
				in = 0
			} else {
				b.WriteRune(r)
			}
		case in == '"':
			if r == '"' {
				in = 0
			} else {
				b.WriteRune(r)
			}
		case r == '\'' || r == '"':
			in, quoted = r, true
		case r == ' ' || r == '\t':
			if b.Len() > 0 || quoted {
				out = append(out, b.String())
				b.Reset()
				quoted = false
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 || quoted {
		out = append(out, b.String())
	}
	return out
}

// simpleCommands are the commands a shell line runs, each as its words with
// leading variable assignments and runners (sudo, env, nice, time) taken off.
func simpleCommands(cmd string) [][]string {
	var out [][]string
	for _, part := range cmdSplit.Split(cmd, -1) {
		if words := unwrap(shellWords(strings.TrimSpace(part))); len(words) > 0 {
			out = append(out, words)
		}
	}
	return out
}

// unwrap takes off what runs a command for it, so the command itself is
// judged: variable assignments, runners (sudo, env, nice, time, command,
// nohup, exec, timeout) and launchers (npx, bunx, uv run, poetry run,
// pipenv run, hatch run, pnpm exec and dlx, yarn exec and dlx, bundle exec).
func unwrap(words []string) []string {
	flags := func() {
		for len(words) > 0 && strings.HasPrefix(words[0], "-") {
			words = words[1:]
		}
	}
	for len(words) > 0 {
		w := words[0]
		if strings.Contains(w, "=") && !strings.HasPrefix(w, "=") && !strings.HasPrefix(w, "-") {
			words = words[1:]
			continue
		}
		next := ""
		if len(words) > 1 {
			next = words[1]
		}
		switch filepath.Base(w) {
		case "sudo", "env", "nice", "time", "command", "nohup", "exec", "npx", "bunx":
			words = words[1:]
			flags()
			continue
		case "timeout":
			words = words[1:]
			flags()
			if len(words) > 0 {
				words = words[1:] // the duration
			}
			continue
		case "uv", "poetry", "pipenv", "hatch":
			if next == "run" {
				words = words[2:]
				flags()
				continue
			}
		case "pnpm", "yarn":
			if next == "exec" || next == "dlx" {
				words = words[2:]
				flags()
				continue
			}
		case "bundle":
			if next == "exec" {
				words = words[2:]
				continue
			}
		}
		break
	}
	return words
}

// commandKind says whether one simple command runs tests ("test"), a build,
// vet or lint ("build"), or neither (""), by its program and subcommand.
func commandKind(words []string) string {
	if len(words) == 0 {
		return ""
	}
	prog := filepath.Base(words[0])
	sub, third := "", ""
	if len(words) > 1 {
		sub = words[1]
	}
	if len(words) > 2 {
		third = words[2]
	}
	switch prog {
	case "go":
		switch sub {
		case "test":
			return "test"
		case "build", "vet", "install":
			return "build"
		}
	case "cargo":
		switch sub {
		case "test", "nextest":
			return "test"
		case "build", "check", "clippy":
			return "build"
		}
	case "npm", "pnpm", "yarn", "bun":
		script := sub
		if sub == "run" {
			script = third
		}
		switch {
		case script == "test" || script == "t" || strings.HasPrefix(script, "test:"):
			return "test"
		case script == "build" || strings.HasPrefix(script, "build:") || script == "lint" || script == "typecheck":
			return "build"
		case prog != "npm" && sub != "run" && len(words) > 1:
			return commandKind(words[1:]) // yarn jest: the binary it runs
		}
	case "make", "gmake":
		switch sub {
		case "test", "check", "tests":
			return "test"
		case "", "build", "all", "lint", "vet":
			return "build"
		}
	case "pytest", "jest", "vitest", "mocha", "rspec", "phpunit", "ctest":
		return "test"
	case "python", "python3":
		if sub == "-m" && (third == "pytest" || third == "unittest") {
			return "test"
		}
	case "tsc", "eslint", "ruff", "mypy", "flake8", "pylint", "golangci-lint", "shellcheck", "javac", "gcc", "clang":
		return "build"
	case "mvn", "gradle", "./gradlew", "gradlew":
		switch sub {
		case "test", "verify", "check":
			return "test"
		case "build", "package", "compile", "assemble":
			return "build"
		}
	case "dotnet":
		switch sub {
		case "test":
			return "test"
		case "build":
			return "build"
		}
	}
	return ""
}

// commandRuns reports whether a shell line runs a command of the kind: a
// test run, a build, or for "" any command at all.
func commandRuns(cmd, kind string) bool {
	if kind == "" {
		return strings.TrimSpace(cmd) != ""
	}
	for _, w := range simpleCommands(cmd) {
		k := commandKind(w)
		// A build claim is met by a test run too: tests build first.
		if k == kind || (kind == "build" && k == "test") {
			return true
		}
	}
	return false
}

// writeTargets are the files a shell line names as written: redirections,
// tee, the destination of cp, mv, install and ln, what rm, touch, mkdir and
// truncate name, sed -i's and perl -i's files, sort -o, gofmt -w and the
// formatters given files, and git's checkout, restore, rm and mv. Other words
// it names, such as a cp source, are not targets. A relative target is taken
// from the folder an earlier cd on the line moved to.
//
// known is false when some command on the line may write files it does not
// name: a program that is neither known to only read nor one of these, such
// as a script, git apply or patch. Nothing it might have changed can then be
// ruled out.
func writeTargets(cmd string) (out []string, known bool) {
	known = true
	dir := ""
	for _, words := range simpleCommands(cmd) {
		args := []string{}
		var targets []string
		for i := 0; i < len(words); i++ {
			w := words[i]
			if j := strings.Index(w, ">"); j >= 0 {
				if pre := strings.TrimRight(w[:j], "0123456789&"); pre != "" {
					args = append(args, pre) // echo x>>file: x is an argument
				}
				t := strings.TrimLeft(w[j:], ">")
				if t == "" && i+1 < len(words) {
					i++
					t = words[i]
				}
				if t != "" && !strings.HasPrefix(t, "&") && t != "/dev/null" {
					targets = append(targets, t)
				}
				continue
			}
			if strings.HasPrefix(w, "<") {
				if w == "<" || w == "<<" || w == "<<<" {
					i++
				}
				continue
			}
			args = append(args, w)
		}
		if len(args) > 0 {
			t, ok := namedWrites(args)
			targets = append(targets, t...)
			if args[0] == "cd" {
				if len(args) > 1 {
					dir = filepath.Join(dir, args[1])
					if filepath.IsAbs(args[1]) {
						dir = args[1]
					}
				}
			} else if !ok {
				known = false
			}
		}
		for _, t := range targets {
			if dir != "" && !filepath.IsAbs(t) && !strings.HasPrefix(t, "~") {
				t = filepath.Join(dir, t)
			}
			out = append(out, t)
		}
	}
	return out, known
}

// namedWrites is what one simple command writes, by name, and whether that
// is all it can write: false for a program that is not known to.
func namedWrites(args []string) ([]string, bool) {
	operands := func(from int) []string {
		var ops []string
		for _, a := range args[from:] {
			if !strings.HasPrefix(a, "-") {
				ops = append(ops, a)
			}
		}
		return ops
	}
	has := func(flags ...string) bool {
		for _, a := range args[1:] {
			for _, f := range flags {
				if a == f || strings.HasPrefix(a, f+"=") {
					return true
				}
			}
		}
		return false
	}
	prog := filepath.Base(args[0])
	switch prog {
	case "tee", "rm", "touch", "mkdir", "truncate", "rmdir", "unlink", "shred":
		return operands(1), true
	case "cp", "mv", "install", "ln", "rsync":
		if ops := operands(1); len(ops) >= 2 {
			return ops[len(ops)-1:], true
		}
		return nil, false
	case "sed", "perl":
		inPlace, script := false, true
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "-i") || a == "--in-place" || (prog == "perl" && strings.HasPrefix(a, "-") && strings.Contains(a, "i")) {
				inPlace = true
			}
			if strings.HasPrefix(a, "-e") {
				script = false
			}
		}
		if !inPlace {
			return nil, true
		}
		ops := operands(1)
		if script && len(ops) > 0 {
			ops = ops[1:]
		}
		return ops, true
	case "sort":
		for i, a := range args {
			if a == "-o" && i+1 < len(args) {
				return []string{args[i+1]}, true
			}
			if v, ok := strings.CutPrefix(a, "--output="); ok {
				return []string{v}, true
			}
		}
		return nil, true
	case "gofmt", "goimports":
		if has("-w") {
			return operands(1), true
		}
		return nil, true
	case "prettier":
		if has("--write", "-w") {
			return operands(1), true
		}
		return nil, true
	case "black", "isort", "rustfmt", "clang-format":
		if prog == "clang-format" && !has("-i") {
			return nil, true
		}
		return operands(1), true
	case "ruff":
		if len(args) > 1 && args[1] == "format" {
			return operands(2), true
		}
	case "git":
		if len(args) < 2 {
			return nil, true
		}
		switch args[1] {
		case "checkout", "restore", "rm", "mv":
			ops := operands(2)
			if args[1] == "mv" && len(ops) >= 2 {
				ops = ops[len(ops)-1:]
			}
			return ops, true
		case "add", "commit", "push", "fetch", "tag", "status", "diff", "log", "show", "branch", "remote", "config":
			return nil, true // nothing in the working tree
		}
		return nil, false // apply, pull, merge, reset, stash and the rest
	case "echo", "printf", "cd", "true", "false", ":":
		return nil, true
	}
	if readOnlyProgram(prog, sub1(args), args) {
		return nil, true
	}
	return nil, false
}

func sub1(args []string) string {
	if len(args) > 1 {
		return args[1]
	}
	return ""
}

// pathIs reports whether target names the file p, a path the claim gave, on
// whole path segments: p is the target, or its tail after a slash.
func pathIs(target, p string) bool {
	t := normText(filepath.ToSlash(filepath.Clean(target)))
	p = strings.TrimPrefix(normText(filepath.ToSlash(filepath.Clean(p))), "./")
	return t != "" && p != "" && (t == p || strings.HasSuffix(t, "/"+p))
}

// runsCommand reports whether a shell line runs claimed, a command the claim
// quotes, as one of its simple commands: the same words, or those words and
// more arguments after them.
func runsCommand(line, claimed string) bool {
	want := strings.Join(shellWords(normText(claimed)), " ")
	if want == "" {
		return false
	}
	for _, w := range simpleCommands(normText(line)) {
		got := strings.Join(w, " ")
		if got == want || strings.HasPrefix(got, want+" ") {
			return true
		}
	}
	return false
}

// sameProgram reports whether a shell line runs the program and subcommand
// the claimed command starts with, whatever the other arguments.
func sameProgram(line, claimed string) bool {
	want := unwrap(shellWords(normText(claimed)))
	if len(want) == 0 {
		return false
	}
	for _, w := range simpleCommands(normText(line)) {
		if filepath.Base(w[0]) != filepath.Base(want[0]) {
			continue
		}
		if len(want) == 1 || (len(w) > 1 && w[1] == want[1]) {
			return true
		}
	}
	return false
}
