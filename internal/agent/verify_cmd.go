package agent

import (
	"bytes"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// Reading a shell line the way the answer checks need it, on the shell
// parser policy uses: the simple commands it runs, which of them only read,
// which write and which files, and where. Both defaults lean to the safe
// side. A command counts as changing nothing only when it is a known reader
// in its reading form (verify_readers.go); anything else is a change, and its
// files are known only when it is a known writer the parser reads in full.

// simpleCmd is one simple command of a line.
type simpleCmd struct {
	// words are its literal words, runners taken off; nil when its program
	// is not a literal.
	words []string
	// pure: a known reader in its reading form, with no output redirected.
	pure bool
	// writes: it may change something. known: every file it may change is
	// in targets.
	writes, known bool
	targets       []string
}

// lineFacts is what a whole shell line does.
type lineFacts struct {
	cmds []simpleCmd
	// parsed is false for a line the parser refused: nothing about it is known.
	parsed bool
	// inspect: every command only reads, so the line changes nothing.
	inspect bool
	// known: every change the line may make is in targets, each resolved
	// against the folder it ran in, absolute where that folder is known.
	known   bool
	targets []string
	// selfStale: a command that changes something runs after one whose
	// output the result shows, so that output may predate the change.
	selfStale bool
	// cdTo is where a line that is only `cd <folder>` leaves the shell.
	cdTo string
}

// maxLineBytes bounds what the checks parse; a longer line is not read.
const maxLineBytes = 64 << 10

// readLine reads a shell line run in cwd; "" for a folder not known, which
// leaves relative targets unresolved.
func readLine(line, cwd string) lineFacts {
	lf := lineFacts{}
	if len(line) > maxLineBytes {
		return lf
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(line), "")
	if err != nil {
		return lf
	}
	lf.parsed = true
	r := &lineReader{cwd: cwd, lf: &lf}
	r.stmts(file.Stmts, true)
	lf.inspect, lf.known = true, true
	seenOutput := false
	for _, c := range lf.cmds {
		switch {
		case c.writes:
			lf.inspect = false
			lf.known = lf.known && c.known
			lf.selfStale = lf.selfStale || seenOutput
		case c.pure:
			seenOutput = true
		}
	}
	if len(file.Stmts) == 1 && len(lf.cmds) == 1 && r.loneCd != "" {
		lf.cdTo = r.loneCd
	}
	return lf
}

type lineReader struct {
	cwd string
	lf  *lineFacts
	// unresolved: a change of folder the reader could not follow, so relative
	// targets after it are not known.
	unresolved bool
	loneCd     string
}

func (r *lineReader) stmts(list []*syntax.Stmt, top bool) {
	for _, st := range list {
		r.stmt(st, top)
	}
}

func (r *lineReader) stmt(st *syntax.Stmt, top bool) {
	switch c := st.Cmd.(type) {
	case *syntax.CallExpr:
		r.call(st, c, top)
	case *syntax.BinaryCmd:
		pipe := c.Op == syntax.Pipe || c.Op == syntax.PipeAll
		r.stmt(c.X, top && !pipe)
		r.stmt(c.Y, top && !pipe)
	case *syntax.Block:
		r.stmts(c.Stmts, top)
		r.redirectsOnly(st)
	case *syntax.Subshell:
		r.stmts(c.Stmts, false)
		r.redirectsOnly(st)
	case *syntax.TimeClause:
		if c.Stmt != nil {
			r.stmt(c.Stmt, top)
		}
		r.redirectsOnly(st)
	default:
		// Loops, conditionals, functions and the rest: what they do is not
		// read, so they count as changing anything.
		r.add(simpleCmd{writes: true})
	}
}

// redirectsOnly records a group's own redirections as a write.
func (r *lineReader) redirectsOnly(st *syntax.Stmt) {
	if targets, ok, some := redirectTargets(st); some {
		r.add(simpleCmd{writes: true, known: ok, targets: targets})
	}
}

func (r *lineReader) add(c simpleCmd) {
	for i, t := range c.targets {
		switch {
		case strings.HasPrefix(t, "~"):
			c.known = false
		case filepath.IsAbs(t):
			c.targets[i] = filepath.Clean(t)
		case r.cwd == "" || r.unresolved:
			c.known = false
		default:
			c.targets[i] = filepath.Join(r.cwd, t)
		}
	}
	r.lf.cmds = append(r.lf.cmds, c)
	if c.writes {
		r.lf.targets = append(r.lf.targets, c.targets...)
	}
}

func (r *lineReader) call(st *syntax.Stmt, call *syntax.CallExpr, top bool) {
	if len(call.Args) == 0 {
		r.redirectsOnly(st)
		return // assignments alone
	}
	// A command or process substitution runs a command the words hide.
	hidden := false
	syntax.Walk(call, func(n syntax.Node) bool {
		switch n.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			hidden = true
		}
		return !hidden
	})
	words := make([]string, 0, len(call.Args))
	literalAll := true
	for _, w := range call.Args {
		v, ok := wordLiteral(w)
		literalAll = literalAll && ok
		words = append(words, v)
	}
	if prog, ok := wordLiteral(call.Args[0]); hidden || !ok || prog == "" {
		r.add(simpleCmd{writes: true})
		return
	}
	words = unwrap(words)
	if len(words) == 0 {
		r.add(simpleCmd{pure: true})
		return
	}
	switch filepath.Base(words[0]) {
	case "cd", "pushd", "popd":
		if words[0] == "cd" && len(words) == 2 && literalAll && top && !strings.HasPrefix(words[1], "-") && !strings.HasPrefix(words[1], "~") {
			switch {
			case filepath.IsAbs(words[1]):
				r.cwd = filepath.Clean(words[1])
			case r.cwd != "":
				r.cwd = filepath.Join(r.cwd, words[1])
			}
			r.loneCd = words[1]
		} else {
			r.unresolved = true
		}
		r.add(simpleCmd{words: words, pure: true})
		return
	}
	redirected, redirectKnown, someRedirect := redirectTargets(st)
	c := simpleCmd{words: words}
	if literalAll && pureReader(words) {
		c.pure, c.writes = !someRedirect, someRedirect
		c.known, c.targets = redirectKnown, redirected
	} else {
		c.writes = true
		if targets, ok := knownWrites(st, words, literalAll); ok && redirectKnown {
			targets = append(targets, redirected...)
			c.known, c.targets = true, targets
		}
	}
	r.add(c)
}

// redirectTargets are the files a statement's own output redirections write.
// ok is false when one is named by an expansion; some reports one at all.
func redirectTargets(st *syntax.Stmt) (targets []string, ok, some bool) {
	ok = true
	for _, rd := range st.Redirs {
		switch rd.Op {
		case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll, syntax.RdrClob, syntax.AppClob, syntax.RdrInOut:
		default:
			continue
		}
		v, lit := wordLiteral(rd.Word)
		if !lit {
			some, ok = true, false
			continue
		}
		if v == "/dev/null" || v == "/dev/stdout" || v == "/dev/stderr" {
			continue
		}
		some = true
		targets = append(targets, v)
	}
	return targets, ok, some
}

// knownWrites is what a writer the checks know writes, when it can be read in
// full: the copying, teeing and in-place editing commands policy reads, and
// the few others whose operands are the files they change.
func knownWrites(st *syntax.Stmt, words []string, literalAll bool) ([]string, bool) {
	if !literalAll {
		return nil, false
	}
	switch filepath.Base(words[0]) {
	case "sed", "gsed", "perl":
		if !hasOpt(words, "i", "--in-place") {
			return nil, false // not in place, and not a pure reader: a script that may write
		}
		return inPlaceFiles(words)
	case "cp", "mv", "ln", "install", "tee", "dd", "curl", "wget":
		// Policy's own reading of what these write, quoting and options
		// included: the command is printed back for it whole.
		var b bytes.Buffer
		if err := syntax.NewPrinter().Print(&b, &syntax.Stmt{Cmd: st.Cmd}); err != nil {
			return nil, false
		}
		canon := tools.CanonicalCommand(b.String())
		if canon.Unparsed() || canon.WriteHidden() != "" || canon.MovesFolder() {
			return nil, false
		}
		return canon.Writes(), true
	case "rm", "rmdir", "unlink", "touch", "mkdir", "truncate", "shred":
		return operands(words), true
	case "gofmt", "goimports":
		if hasOpt(words, "w", "") {
			return operands(words), true
		}
	}
	return nil, false
}

// inPlaceFiles are the files sed -i or perl -i edits: its operands, less the
// script when no -e names it, and less the values of -e, -f and their kin.
func inPlaceFiles(words []string) ([]string, bool) {
	perl := filepath.Base(words[0]) == "perl"
	var ops []string
	script := false // a script given by option
	ended := false
	for i := 1; i < len(words); i++ {
		a := words[i]
		switch {
		case ended || !strings.HasPrefix(a, "-") || a == "-":
			ops = append(ops, a)
		case a == "--":
			ended = true
		case a == "-e" || a == "-f" || a == "--expression" || a == "--file" || (perl && a == "-E"):
			script = true
			i++
		case strings.HasPrefix(a, "--expression=") || strings.HasPrefix(a, "--file="):
			script = true
		case !strings.HasPrefix(a, "--") && (strings.ContainsAny(a[1:], "ef") && !perl || perl && strings.ContainsAny(a[1:], "eE")):
			// -ne 's/a/b/', -pie: the cluster's last letter takes the next word.
			script = true
			if last := a[len(a)-1]; last == 'e' || last == 'f' || last == 'E' {
				i++
			}
		}
	}
	if !script {
		if len(ops) == 0 {
			return nil, false
		}
		ops = ops[1:]
	}
	return ops, len(ops) > 0
}

// operands are a command's words after its program that are not options.
func operands(words []string) []string {
	var ops []string
	ended := false
	for _, a := range words[1:] {
		switch {
		case a == "--" && !ended:
			ended = true
		case ended || !strings.HasPrefix(a, "-") || a == "-":
			ops = append(ops, a)
		}
	}
	return ops
}

// hasOpt reports a short option letter, alone or in a cluster, or a long
// option, before any "--".
func hasOpt(words []string, short, long string) bool {
	for _, a := range words[1:] {
		switch {
		case a == "--":
			return false
		case long != "" && (a == long || strings.HasPrefix(a, long+"=")):
			return true
		case short != "" && len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.Contains(a[1:], short):
			return true
		}
	}
	return false
}

// wordLiteral is a word's value when it holds no expansion, quotes taken off
// as the shell takes them.
func wordLiteral(w *syntax.Word) (string, bool) {
	if w == nil {
		return "", false
	}
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(unescapeLit(p.Value))
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, in := range p.Parts {
				l, ok := in.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`, "\\`", "`").Replace(l.Value))
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// unescapeLit undoes backslashes in an unquoted literal.
func unescapeLit(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// unwrap takes off what runs a command for it, so the command itself is
// judged: variable assignments, runners (sudo, env, nice, time, command,
// nohup, exec, timeout, xargs) and launchers (npx, bunx, uv run, poetry run,
// pipenv run, hatch run, pnpm exec and dlx, yarn exec and dlx, bundle exec).
// A runner given an option that changes the folder, the user or how the
// command is read is left on, and the command is not judged as a reader.
func unwrap(words []string) []string {
	flags := func() bool {
		for len(words) > 0 && strings.HasPrefix(words[0], "-") {
			if w := words[0]; w == "-C" || strings.HasPrefix(w, "--chdir") || w == "-D" || w == "-u" || strings.HasPrefix(w, "--user") ||
				w == "-S" || strings.HasPrefix(w, "--split-string") || w == "-I" || w == "-i" {
				return false
			}
			words = words[1:]
		}
		return true
	}
	for len(words) > 0 {
		w := words[0]
		if name, _, assign := strings.Cut(w, "="); assign && name != "" && !strings.HasPrefix(w, "-") && !strings.Contains(name, "/") {
			words = words[1:]
			continue
		}
		next := ""
		if len(words) > 1 {
			next = words[1]
		}
		switch filepath.Base(w) {
		case "sudo", "env", "nice", "time", "command", "nohup", "exec", "npx", "bunx", "xargs":
			words = words[1:]
			if !flags() {
				return []string{w, "--runner-option"}
			}
			continue
		case "timeout":
			words = words[1:]
			if !flags() {
				return []string{w, "--runner-option"}
			}
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

// simpleCommands are the literal simple commands of a line, runners taken
// off; a command whose program is not a literal is left out.
func simpleCommands(line string) [][]string {
	var out [][]string
	for _, c := range readLine(line, "").cmds {
		if len(c.words) > 0 {
			out = append(out, c.words)
		}
	}
	return out
}

// inspectionCommand reports whether a shell line only reads: every simple
// command in it is a known reader in its reading form, nothing is redirected
// to a file, and the parser read it all.
func inspectionCommand(cmd string) bool {
	lf := readLine(cmd, "")
	return lf.parsed && strings.TrimSpace(cmd) != "" && lf.inspect
}

// pathIs reports whether target names the file p a claim gave: the same
// absolute path, p relative to workDir, or p's segments ending target.
func pathIs(target, p, workDir string) bool {
	if target == "" || p == "" {
		return false
	}
	t := filepath.Clean(filepath.ToSlash(target))
	p = filepath.ToSlash(p)
	if filepath.IsAbs(p) {
		return t == filepath.Clean(p)
	}
	q := strings.TrimPrefix(filepath.Clean(p), "./")
	if workDir != "" && filepath.IsAbs(t) {
		// A short name may stand for a file anywhere in the workspace, not
		// outside it.
		w := filepath.Clean(workDir)
		return t == filepath.Join(w, q) || strings.HasPrefix(t, w+"/") && strings.HasSuffix(t, "/"+q)
	}
	return t == q || strings.HasSuffix(t, "/"+q)
}

// claimedCommand is the command a claim quotes, read as a line, unwrapped.
func claimedCommand(claimed string) []string {
	cmds := simpleCommands(claimed)
	if len(cmds) != 1 {
		return nil
	}
	return cmds[0]
}

// runsCommand reports whether a shell line runs the command a claim quotes,
// as one of its simple commands: the same words, or those words and more
// arguments after them, runners taken off both.
func runsCommand(line, claimed string) bool {
	want := claimedCommand(claimed)
	if len(want) == 0 {
		return false
	}
	for _, w := range simpleCommands(line) {
		if len(w) >= len(want) && equalWords(w[:len(want)], want) {
			return true
		}
	}
	return false
}

// sameProgram reports whether a shell line runs the program and subcommand
// the claimed command starts with, whatever the other arguments.
func sameProgram(line, claimed string) bool {
	want := claimedCommand(claimed)
	if len(want) == 0 {
		return false
	}
	for _, w := range simpleCommands(line) {
		if filepath.Base(w[0]) != filepath.Base(want[0]) {
			continue
		}
		if len(want) == 1 || (len(w) > 1 && w[1] == want[1]) {
			return true
		}
	}
	return false
}

func equalWords(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] && (i != 0 || filepath.Base(a[i]) != filepath.Base(b[i])) {
			return false
		}
	}
	return true
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
