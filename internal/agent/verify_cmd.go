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
// which write and which files, and where. Every default leans to the safe
// side: a command counts as changing nothing only when it is a known reader
// (verify_readers.go), and its files are known only when it is a known writer
// whose files the parser reads in full. An unknown leaves a claim unverified,
// never verified and never contradicted.

// simpleCmd is one simple command of a line.
type simpleCmd struct {
	// words are its literal words, runners taken off; nil when it is not
	// a literal simple command. raw are its words as written, assignments
	// included; wrapped says a runner or an assignment was taken off.
	words, raw []string
	wrapped    bool
	// assigns are the names of the variables set for it, before it or by env.
	assigns []string
	// first: the line's first command, at its top, which runs whatever
	// follows it.
	first bool
	// covers are folders a formatter rewrote: every file beneath them.
	covers []string
	// pure: a known reader, with no output redirected. cd: a change of folder.
	pure, cd bool
	// writes: it may change something. known: every file it may change is
	// in targets, resolved against the folder it ran in.
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
	// known: every change the line may make is in targets.
	known   bool
	targets []string
	// selfStale: a command that changes something runs after one whose
	// output the result shows, so that output may predate the change.
	selfStale bool
	// shape: the line is one simple command, or a chain of them joined by
	// && alone: no pipe, ;, ||, & or group. Its exit status is then the last
	// command's, and an exit of 0 means every command ran and succeeded.
	shape bool
	// final is the shaped line's last command; leadCd that every command
	// before it is a cd; finalCwd the folder it ran in, "" when not known;
	// finalWrapped that a runner or an assignment was taken off it.
	final        []string
	leadCd       bool
	finalCwd     string
	finalWrapped bool
	finalAssigns []string
	// covers are the folders formatters rewrote.
	covers []string
	// writer: a shaped line of literal commands, each a cd, a reader or a
	// known writer, whose every file is known.
	writer bool
}

// maxLineBytes bounds what the checks parse; a longer line is not read.
const maxLineBytes = 64 << 10

// readLine reads a shell line run in cwd, the folder the bash tool used;
// "" leaves relative files unresolved.
func readLine(line, cwd string) lineFacts {
	lf := lineFacts{}
	if len(line) > maxLineBytes {
		return lf
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(line), "")
	if err != nil || len(file.Stmts) == 0 {
		return lf
	}
	lf.parsed = true
	r := &lineReader{cwd: cwd, lf: &lf, shape: len(file.Stmts) == 1}
	r.stmts(file.Stmts, len(file.Stmts) == 1)
	lf.inspect, lf.known = true, true
	seenOutput := false
	for _, c := range lf.cmds {
		switch {
		case c.writes:
			lf.inspect = false
			lf.known = lf.known && c.known
			lf.selfStale = lf.selfStale || seenOutput
		case c.pure && !c.cd:
			seenOutput = true
		}
	}
	if r.shape && len(lf.cmds) > 0 {
		lf.shape = true
		last := lf.cmds[len(lf.cmds)-1]
		lf.final, lf.finalWrapped, lf.finalAssigns = last.words, last.wrapped, last.assigns
		if !r.unresolved {
			lf.finalCwd = r.cwd
		}
		lf.leadCd = true
		lf.writer = lf.known
		for i, c := range lf.cmds {
			if c.words == nil {
				lf.writer = false
			}
			if i < len(lf.cmds)-1 && !c.cd {
				lf.leadCd = false
			}
		}
	}
	return lf
}

type lineReader struct {
	cwd string
	lf  *lineFacts
	// shape stays true while the line is one && chain of simple commands.
	shape bool
	// unresolved: a change of folder that could not be followed, so the
	// relative files after it are not known.
	unresolved bool
	// top is set while the statement being read is at the top of the line.
	top bool
}

func (r *lineReader) stmts(list []*syntax.Stmt, follow bool) {
	for _, st := range list {
		r.stmt(st, follow)
	}
}

// stmt reads one statement. follow is true where a cd carries on to what
// comes after it: the top of the line and the parts of an && chain there.
func (r *lineReader) stmt(st *syntax.Stmt, follow bool) {
	if st.Background || st.Coprocess || st.Negated {
		r.shape = false
	}
	r.top = follow
	switch c := st.Cmd.(type) {
	case *syntax.CallExpr:
		r.call(st, c, follow)
	case *syntax.BinaryCmd:
		and := c.Op == syntax.AndStmt
		if !and {
			r.shape = false
		}
		r.stmt(c.X, follow && and)
		r.stmt(c.Y, follow && and)
	case *syntax.Block:
		r.shape = false
		r.stmts(c.Stmts, false)
		r.redirectsOnly(st)
	case *syntax.Subshell:
		r.shape = false
		r.stmts(c.Stmts, false)
		r.redirectsOnly(st)
	case *syntax.TimeClause:
		r.shape = false
		if c.Stmt != nil {
			r.stmt(c.Stmt, false)
		}
		r.redirectsOnly(st)
	default:
		// Loops, conditionals, functions and the rest: what they do is not
		// read, so they count as changing anything.
		r.shape = false
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
	for i, t := range c.covers {
		switch {
		case filepath.IsAbs(t):
			c.covers[i] = filepath.Clean(t)
		case r.cwd == "" || r.unresolved:
			c.covers[i] = ""
		default:
			c.covers[i] = filepath.Join(r.cwd, t)
		}
	}
	c.first = len(r.lf.cmds) == 0 && r.top
	r.lf.cmds = append(r.lf.cmds, c)
	if c.writes {
		r.lf.targets = append(r.lf.targets, c.targets...)
		r.lf.covers = append(r.lf.covers, c.covers...)
	}
}

func (r *lineReader) call(st *syntax.Stmt, call *syntax.CallExpr, follow bool) {
	redirected, redirectKnown, someRedirect := redirectTargets(st)
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
	literalAll := !hidden
	var raw []string
	for _, a := range call.Assigns {
		if a.Value == nil {
			literalAll = false
			continue
		}
		v, ok := wordLiteral(a.Value)
		literalAll = literalAll && ok && !a.Append && a.Index == nil && a.Array == nil
		raw = append(raw, a.Name.Value+"="+v)
	}
	for _, w := range call.Args {
		v, ok := wordLiteral(w)
		literalAll = literalAll && ok
		words = append(words, v)
	}
	if !literalAll {
		r.add(simpleCmd{writes: true})
		return
	}
	raw = append(raw, words...)
	unwrapped := unwrap(words)
	// An assignment, before the command or given to env, can change what
	// it does (UPDATE=1 go test rewrites goldens): never a reader then.
	assigned := len(call.Assigns) > 0 || hasAssignment(words, unwrapped)
	wrapped := assigned || len(unwrapped) != len(words)
	words = unwrapped
	if len(words) == 0 {
		// A runner alone, as env, writes only where it is redirected.
		r.add(simpleCmd{raw: raw, pure: !someRedirect && !assigned, writes: someRedirect || assigned, known: redirectKnown && !assigned, targets: redirected})
		return
	}
	switch filepath.Base(words[0]) {
	case "cd", "pushd", "popd":
		if assigned {
			r.unresolved = true // CDPATH=… cd: not the folder it names
		} else {
			r.cd(words, follow)
		}
		r.add(simpleCmd{words: words, raw: raw, pure: !someRedirect, cd: true, writes: someRedirect, known: redirectKnown, targets: redirected})
		return
	}
	c := simpleCmd{words: words, raw: raw, wrapped: wrapped, assigns: assignedNames(call, raw, words)}
	if assigned {
		c.writes = true
		r.add(c)
		return
	}
	if pureReader(words) {
		c.pure, c.writes = !someRedirect, someRedirect
		c.known, c.targets = redirectKnown, redirected
	} else {
		c.writes = true
		if targets, ok := knownWrites(st, words); ok && redirectKnown {
			if p := filepath.Base(words[0]); p == "gofmt" || p == "goimports" {
				c.covers = append([]string(nil), targets...)
			}
			targets = append(targets, redirected...)
			c.known, c.targets = true, targets
		}
	}
	r.add(c)
}

// cd follows a change of folder where it carries on: `cd <folder>` with one
// literal folder, at the top of the line or in an && chain there. Anything
// else (cd alone, cd ~, cd -, pushd, popd, a cd after ; or ||) leaves the
// folder unknown for the rest of the line.
func (r *lineReader) cd(words []string, follow bool) {
	if !follow || words[0] != "cd" || len(words) != 2 || r.cwd == "" || r.unresolved {
		r.unresolved = true
		return
	}
	dir := words[1]
	switch {
	case dir == "" || strings.HasPrefix(dir, "-") || strings.HasPrefix(dir, "~"):
		r.unresolved = true
	case dir == ".." || strings.HasPrefix(dir, "../") || strings.Contains(dir, "/../") || strings.HasSuffix(dir, "/.."):
		r.unresolved = true // .. after a link is not the lexical parent
	case filepath.IsAbs(dir):
		r.cwd = filepath.Clean(dir)
	default:
		r.cwd = filepath.Join(r.cwd, dir)
	}
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

// writerSpecs are the plain writers whose operands are the files they change,
// with the options each may be given (values not taken as operands); any
// other option leaves the files unknown.
var writerSpecs = map[string]readerSpec{
	"rm": {short: "rRfidvI", long: set("--recursive", "--force", "--dir", "--verbose", "--one-file-system",
		"--preserve-root", "--no-preserve-root"), maxOperands: -1},
	"rmdir":  {short: "pv", long: set("--parents", "--verbose", "--ignore-fail-on-non-empty"), maxOperands: -1},
	"unlink": {maxOperands: -1},
	"touch": {short: "acmfhrtd", shortValued: "rtd", long: set("--no-create", "--reference", "--date", "--time",
		"--no-dereference"), longValued: set("--reference", "--date", "--time"), maxOperands: -1},
	"mkdir": {short: "pvm", shortValued: "m", long: set("--parents", "--verbose", "--mode"), longValued: set("--mode"), maxOperands: -1},
	"truncate": {short: "cors", shortValued: "rs", long: set("--no-create", "--io-blocks", "--reference", "--size"),
		longValued: set("--reference", "--size"), maxOperands: -1},
	"shred": {short: "fnszuvx", shortValued: "ns", long: set("--force", "--iterations", "--size", "--remove", "--verbose",
		"--exact", "--zero"), longValued: set("--iterations", "--size"), maxOperands: -1},
}

// knownWrites is what a writer the checks know writes, when it can be read in
// full: the copying and teeing commands policy reads, sed -i and perl -i, the
// plain writers above, and gofmt -w and goimports -w (whose folders count as
// every file beneath them).
func knownWrites(st *syntax.Stmt, words []string) ([]string, bool) {
	prog := filepath.Base(words[0])
	switch prog {
	case "sed", "gsed", "perl":
		return inPlaceFiles(words)
	case "cp", "mv", "ln", "install", "tee", "dd":
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
	case "gofmt", "goimports":
		return formatterFiles(words)
	}
	if spec, ok := writerSpecs[prog]; ok {
		_, ops, ok := parseReader(spec, words)
		return ops, ok && len(ops) > 0
	}
	return nil, false
}

// formatterFiles are the files and folders gofmt -w or goimports -w rewrites.
func formatterFiles(words []string) ([]string, bool) {
	var ops []string
	write := false
	for i := 1; i < len(words); i++ {
		switch a := words[i]; {
		case a == "-w":
			write = true
		case a == "-l" || a == "-d" || a == "-s" || a == "-e" || a == "-format-only":
		case a == "-r" || a == "-local" || a == "-srcdir":
			i++
		case strings.HasPrefix(a, "-"):
			return nil, false
		default:
			ops = append(ops, a)
		}
	}
	return ops, write && len(ops) > 0
}

// inPlaceFiles are the files sed -i or perl -i edits: its operands, less the
// script when no -e names it, and less the values of -e, -f and their kin. Any
// long option not spelled out in full leaves them unknown.
func inPlaceFiles(words []string) ([]string, bool) {
	perl := filepath.Base(words[0]) == "perl"
	var ops []string
	inPlace, script, ended := false, false, false
	for i := 1; i < len(words); i++ {
		a := words[i]
		switch {
		case ended || !strings.HasPrefix(a, "-") || a == "-":
			ops = append(ops, a)
		case a == "--":
			ended = true
		case a == "--in-place" || strings.HasPrefix(a, "--in-place="):
			inPlace = true
		case a == "--expression" || a == "--file":
			script = true
			i++
		case strings.HasPrefix(a, "--expression=") || strings.HasPrefix(a, "--file="):
			script = true
		case a == "--regexp-extended" || a == "--quiet" || a == "--silent" || a == "--null-data" || a == "--posix" || a == "--separate":
		case strings.HasPrefix(a, "--"):
			return nil, false // perhaps an abbreviation of an option that changes what is written
		default:
			cluster := a[1:]
			if strings.HasPrefix(cluster, "i") {
				inPlace = true // -i or -i.bak: the rest is a suffix
				continue
			}
			takes, valued := "ef", "l"
			if perl {
				takes, valued = "eE", "IMml0"
			}
			for j := 0; j < len(cluster); j++ {
				if cluster[j] == 'i' {
					inPlace = true
					break // -ni.bak: the rest is the suffix
				}
				if strings.IndexByte(takes, cluster[j]) >= 0 {
					script = true
					if j == len(cluster)-1 {
						i++
					}
					break
				}
				if strings.IndexByte(valued, cluster[j]) >= 0 {
					// sed -l N, perl -Ilib, -MModule: the rest, or the next
					// word, is the option's value, not a script or a file.
					if j == len(cluster)-1 && (!perl || cluster[j] == 'I') {
						i++
					}
					break
				}
			}
		}
	}
	if !inPlace {
		return nil, false
	}
	if !script {
		if len(ops) == 0 {
			return nil, false
		}
		ops = ops[1:]
	}
	return ops, len(ops) > 0
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

// hasAssignment reports a NAME=value word taken off by unwrap, as env's.
func hasAssignment(before, after []string) bool {
	for _, w := range before[:len(before)-len(after)] {
		if name, _, ok := strings.Cut(w, "="); ok && name != "" && !strings.HasPrefix(w, "-") {
			return true
		}
	}
	return false
}

// assignedNames are the variables a command is given: before it, or in the
// words a runner such as env took off.
func assignedNames(call *syntax.CallExpr, raw, words []string) []string {
	var names []string
	for _, a := range call.Assigns {
		names = append(names, a.Name.Value)
	}
	args := raw[len(call.Assigns):]
	for _, w := range args[:len(args)-len(words)] {
		if name, _, ok := strings.Cut(w, "="); ok && name != "" && !strings.HasPrefix(w, "-") {
			names = append(names, name)
		}
	}
	return names
}

// unwrap takes off what runs a command for it, so the command itself is
// judged: variable assignments, runners (sudo, env, nice, time, command,
// nohup, exec, timeout) and launchers (npx, bunx, uv run, poetry run, pipenv
// run, hatch run, pnpm exec and dlx, yarn exec and dlx, bundle exec). A
// runner given any option is left on, and the command is then not a reader.
// xargs is not taken off: what it runs takes its words from its input.
func unwrap(words []string) []string {
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
		case "sudo", "env", "nice", "time", "command", "nohup", "exec", "npx", "bunx":
			if strings.HasPrefix(next, "-") {
				return []string{w, next}
			}
			words = words[1:]
			continue
		case "timeout":
			if strings.HasPrefix(next, "-") || len(words) < 3 {
				return words
			}
			words = words[2:]
			continue
		case "uv", "poetry", "pipenv", "hatch":
			if next == "run" && len(words) > 2 && !strings.HasPrefix(words[2], "-") {
				words = words[2:]
				continue
			}
		case "pnpm", "yarn":
			if (next == "exec" || next == "dlx") && len(words) > 2 && !strings.HasPrefix(words[2], "-") {
				words = words[2:]
				continue
			}
		case "bundle":
			if next == "exec" && len(words) > 2 {
				words = words[2:]
				continue
			}
		}
		break
	}
	return words
}

// simpleCommands are the literal simple commands of a line, runners taken
// off; one that is not literal is left out.
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
// command in it is a known reader, nothing is redirected to a file, and the
// parser read it all.
func inspectionCommand(cmd string) bool {
	lf := readLine(cmd, "")
	return lf.parsed && lf.inspect
}

// slashes writes a path with forward slashes, as a claim may give it.
func slashes(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// pathIs compares a file a call changed with the one a claim names. exact is
// set when they are the same file as written (the same absolute path, the
// claim's path in the workspace, a short name for a file in the workspace,
// or a file beneath a folder the call changed); loose when they are the same
// only ignoring case, which is not taken as either answer.
func pathIs(target, p, workDir string) (exact, loose bool) {
	if target == "" || p == "" {
		return false, false
	}
	if samePath(target, p, workDir) {
		return true, false
	}
	return false, samePath(strings.ToLower(target), strings.ToLower(p), strings.ToLower(workDir))
}

// samePath is pathIs's exact comparison.
func samePath(t, q, workDir string) bool {
	t, q = filepath.Clean(t), slashes(q)
	var want string
	switch {
	case filepath.IsAbs(q):
		want = filepath.Clean(q)
	case workDir != "":
		want = filepath.Join(workDir, strings.TrimPrefix(filepath.Clean(q), "./"))
	}
	if want != "" && (t == want || strings.HasPrefix(want, t+"/")) {
		return true
	}
	if filepath.IsAbs(q) {
		return false
	}
	if workDir == "" {
		short := strings.TrimPrefix(filepath.Clean(q), "./")
		return t == short || strings.HasSuffix(t, "/"+short)
	}
	w := filepath.Clean(workDir)
	short := strings.TrimPrefix(filepath.Clean(q), "./")
	return strings.HasPrefix(t, w+"/") && strings.HasSuffix(t, "/"+short)
}

// claimedCommand is the command a claim quotes, as written, assignments
// included; nil unless it is one literal simple command.
func claimedCommand(claimed string) []string {
	lf := readLine(claimed, "")
	if !lf.parsed || len(lf.cmds) != 1 || lf.cmds[0].raw == nil {
		return nil
	}
	return lf.cmds[0].raw
}

// ranIn reports whether a line ran the claimed command, as written, with
// perhaps more arguments after it: as the line's first command, or as any
// command of an && chain whose exit was 0. A command after ;, || or a failed
// && may not have run, and a runner or a path is part of what is compared.
func ranIn(lf lineFacts, exit *int, claimed string) bool {
	want := claimedCommand(claimed)
	if len(want) == 0 || !lf.parsed {
		return false
	}
	all := lf.shape && exit != nil && *exit == 0
	for _, c := range lf.cmds {
		if !c.first && !all {
			continue
		}
		if len(c.raw) >= len(want) && equalWords(c.raw[:len(want)], want) {
			return true
		}
	}
	return false
}

func equalWords(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
