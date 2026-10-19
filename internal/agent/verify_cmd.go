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
		words := shellWords(strings.TrimSpace(part))
	strip:
		for len(words) > 0 {
			w := words[0]
			if strings.Contains(w, "=") && !strings.HasPrefix(w, "=") && !strings.HasPrefix(w, "-") {
				words = words[1:]
				continue
			}
			switch filepath.Base(w) {
			case "sudo", "env", "nice", "time", "command", "nohup", "exec":
				words = words[1:]
				for len(words) > 0 && strings.HasPrefix(words[0], "-") {
					words = words[1:]
				}
				continue
			}
			break strip
		}
		if len(words) > 0 {
			out = append(out, words)
		}
	}
	return out
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
// truncate name, sed -i's and perl -i's files, and git's checkout, restore,
// rm and mv. Other words it names, such as a cp source, are not targets.
func writeTargets(cmd string) []string {
	var out []string
	for _, words := range simpleCommands(cmd) {
		args := []string{}
		for i := 0; i < len(words); i++ {
			w := words[i]
			if t, ok := strings.CutPrefix(strings.TrimLeft(w, "0123456789&"), ">"); ok {
				t = strings.TrimPrefix(t, ">")
				if t == "" && i+1 < len(words) {
					i++
					t = words[i]
				}
				if t != "" && !strings.HasPrefix(t, "&") && t != "/dev/null" {
					out = append(out, t)
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
		if len(args) == 0 {
			continue
		}
		operands := func(from int) []string {
			var ops []string
			for _, a := range args[from:] {
				if !strings.HasPrefix(a, "-") {
					ops = append(ops, a)
				}
			}
			return ops
		}
		switch filepath.Base(args[0]) {
		case "tee", "rm", "touch", "mkdir", "truncate", "rmdir", "unlink", "shred":
			out = append(out, operands(1)...)
		case "cp", "mv", "install", "ln", "rsync":
			if ops := operands(1); len(ops) >= 2 {
				out = append(out, ops[len(ops)-1])
			}
		case "sed", "perl":
			inPlace, script := false, true
			for _, a := range args[1:] {
				if strings.HasPrefix(a, "-i") || a == "--in-place" || (args[0] == "perl" && strings.Contains(a, "i") && strings.HasPrefix(a, "-")) {
					inPlace = true
				}
				if a == "-e" || strings.HasPrefix(a, "-e") {
					script = false
				}
			}
			if ops := operands(1); inPlace && len(ops) > 0 {
				if script {
					ops = ops[1:]
				}
				out = append(out, ops...)
			}
		case "git":
			if len(args) > 1 {
				switch args[1] {
				case "checkout", "restore", "rm", "mv":
					ops := operands(2)
					if args[1] == "mv" && len(ops) >= 2 {
						ops = ops[len(ops)-1:]
					}
					out = append(out, ops...)
				}
			}
		}
	}
	return out
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
