package monitor

import (
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// command is one simple command of a shell line, as the rules read it.
type command struct {
	// words are the command's words with quotes removed; a word whose value
	// only running the line would give (a variable, a substitution) is kept
	// as written, with its leading $ so it is never taken for a name.
	words []string
	// writes are the files its redirections write; reads, those they read.
	writes, reads []string
	// dir is the working directory the command runs in, tracked through the
	// script's cd/pushd/popd; dirKnown is false once a cd went to a target
	// that could not be resolved statically. "" is where the script started.
	dir      string
	dirKnown bool
}

// script is what the rules read from a shell line.
type script struct {
	cmds []command
	// funcs are the functions the line defines; calling one is not running a program.
	funcs map[string]bool
	// setsPath is set when the line changes PATH, so which program runs is unknown.
	setsPath bool
}

// parseScript reads a shell line, starting at the script's own directory.
func parseScript(src string) (script, bool) { return parseScriptAt(src, "") }

// parseScriptAt reads a shell line whose commands start in start ("" is the
// local workspace root, "~" the login's home over ssh). It follows the
// working directory through cd, pushd and popd so a relative target can be
// resolved against the directory it would run in.
func parseScriptAt(src, start string) (script, bool) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil {
		return script{}, false
	}
	s := script{funcs: map[string]bool{}}
	st := &dirState{dir: start, known: true}
	walkStmts(src, f.Stmts, st, &s)
	return s, true
}

// dirState is the working directory as the script walk reaches a command.
type dirState struct {
	dir    string
	known  bool
	prev   string
	prevOK bool
	stack  []string
}

func (d dirState) clone() *dirState {
	c := d
	c.stack = append([]string(nil), d.stack...)
	return &c
}

// walkStmts follows a list of statements in order, tracking the directory and
// collecting each simple command with the directory it runs in.
func walkStmts(src string, stmts []*syntax.Stmt, st *dirState, s *script) {
	for _, stmt := range stmts {
		walkCmd(src, stmt, st, s)
	}
}

func walkCmd(src string, stmt *syntax.Stmt, st *dirState, s *script) {
	switch x := stmt.Cmd.(type) {
	case *syntax.CallExpr:
		simple(src, x, stmt, st, s)
	case *syntax.BinaryCmd:
		if x.Op == syntax.Pipe || x.Op == syntax.PipeAll {
			// Each stage of a pipe runs in a subshell, so a cd in one does
			// not reach the next or outlive the pipe.
			walkCmd(src, x.X, st.clone(), s)
			walkCmd(src, x.Y, st.clone(), s)
			return
		}
		walkCmd(src, x.X, st, s) // && ; || run in the same shell
		walkCmd(src, x.Y, st, s)
	case *syntax.Block:
		walkStmts(src, x.Stmts, st, s) // { …; } keeps the shell's directory
	case *syntax.Subshell:
		walkStmts(src, x.Stmts, st.clone(), s) // ( … ) is its own shell
	case *syntax.DeclClause:
		// export/declare/local/readonly/typeset: a CDPATH among them changes
		// where cd resolves, so the directory is no longer known.
		for _, a := range x.Args {
			if a.Name == nil {
				continue
			}
			if a.Name.Value == "CDPATH" {
				st.known = false
			}
			if a.Name.Value == "PATH" {
				s.setsPath = true
			}
		}
	case *syntax.FuncDecl:
		if x.Name != nil {
			s.funcs[x.Name.Value] = true
		}
		// A function body runs only when called; collect its commands for the
		// rules, on a copy so its cd does not move the caller. A later call to
		// the function makes the directory unknown (see simple).
		walkCmd(src, x.Body, st.clone(), s)
	default:
		// if/while/for/case and the like: the directory is understood only
		// when every branch leaves it where it began and known. Collect the
		// commands on a copy so each is judged in the right directory, and if
		// any branch moved the directory or lost track of it, the directory
		// afterwards is unknown — which branch ran is not known statically.
		before := *st
		inner := st.clone()
		syntax.Walk(stmt.Cmd, func(n syntax.Node) bool {
			if sub, ok := n.(*syntax.Stmt); ok && sub != stmt {
				walkCmd(src, sub, inner, s)
				return false
			}
			return true
		})
		if inner.dir != before.dir || inner.known != before.known {
			st.known = false
		}
	}
}

// simple records one simple command and, for the small set of forms the walk
// understands, moves the tracked directory. Any other form that could change
// the directory or run code that might — a wrapper or runner, env -C, sudo -D,
// CDPATH, a call to a defined function, eval, source or . — leaves the
// directory unknown from here on, so a later relative target is asked about
// rather than wrongly taken as own ground.
func simple(src string, call *syntax.CallExpr, stmt *syntax.Stmt, st *dirState, s *script) {
	var c command
	c.dir, c.dirKnown = st.dir, st.known
	for _, w := range call.Args {
		c.words = append(c.words, wordText(src, w))
	}
	for _, r := range stmt.Redirs {
		switch r.Op {
		case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll, syntax.RdrClob:
			if r.Word != nil {
				c.writes = append(c.writes, wordText(src, r.Word))
			}
		case syntax.RdrIn:
			if r.Word != nil {
				c.reads = append(c.reads, wordText(src, r.Word))
			}
		}
	}
	for _, a := range call.Assigns {
		if a.Name == nil {
			continue
		}
		if a.Name.Value == "PATH" {
			s.setsPath = true
		}
		if a.Name.Value == "CDPATH" { // changes where cd resolves
			st.known = false
		}
	}

	prog, rest := c.program()
	head := ""
	if len(c.words) > 0 {
		head = c.words[0]
	}
	prefix := wrapperPrefix(c.words)

	switch {
	case head == "cd" && len(prefix) == 0: // a bare cd, the only tracked form
		applyCd(st, firstWord(rest))
	case head == "pushd" && len(prefix) == 0:
		st.stack = append(st.stack, st.dir)
		applyCd(st, firstWord(rest))
	case head == "popd" && len(prefix) == 0:
		if n := len(st.stack); n > 0 {
			st.prev, st.prevOK = st.dir, true
			st.dir, st.stack = st.stack[n-1], st.stack[:n-1]
		} else {
			st.known = false // nothing to pop: where it lands is not known
		}
	case chdirFlag(prefix):
		// env -C, sudo -D move the directory for this very command, so its
		// own target runs somewhere the walk did not follow.
		c.dirKnown, st.known = false, false
	case prog == "cd" || prog == "pushd" || prog == "popd":
		st.known = false // reached through a wrapper (command cd, sudo cd): not tracked
	case setsCDPATH(head, c.words):
		st.known = false
	case isRunner(head) || isRunner(prog):
		st.known = false // eval, source, . run code that may cd
	case s.funcs[head] || s.funcs[prog]:
		st.known = false // a function body may cd
	}

	if len(c.words) > 0 || len(c.writes) > 0 || len(c.reads) > 0 {
		s.cmds = append(s.cmds, c)
	}
}

// firstWord is the first operand, or "" when there is none.
func firstWord(args []string) string {
	for _, a := range operands(args) {
		return a
	}
	return ""
}

// wrapperPrefix is the words a command runs through before its program:
// sudo, env, command, nice and the like, with their flags.
func wrapperPrefix(words []string) []string {
	var prefix []string
	w := words
	for len(w) > 0 {
		head := w[0]
		if strings.Contains(head, "=") && !strings.HasPrefix(head, "=") && !strings.HasPrefix(head, "$") {
			prefix = append(prefix, head)
			w = w[1:]
			continue
		}
		if !wrappers[head] {
			break
		}
		prefix = append(prefix, head)
		w = w[1:]
		if head == "timeout" {
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				prefix = append(prefix, w[0])
				w = w[1:]
			}
			if len(w) > 0 {
				prefix = append(prefix, w[0])
				w = w[1:]
			}
			continue
		}
		for len(w) > 0 && strings.HasPrefix(w[0], "-") {
			prefix = append(prefix, w[0])
			if wrapperValueFlags[w[0]] && len(w) > 1 {
				prefix = append(prefix, w[1])
				w = w[1:]
			}
			w = w[1:]
		}
	}
	return prefix
}

// chdirFlag reports a wrapper flag that changes the working directory:
// env -C/--chdir, sudo -D/--chdir.
func chdirFlag(prefix []string) bool {
	for _, w := range prefix {
		switch w {
		case "-C", "--chdir", "-D":
			return true
		}
		if strings.HasPrefix(w, "--chdir=") {
			return true
		}
	}
	return false
}

// setsCDPATH reports a command that sets or exports CDPATH, which changes
// where every later cd resolves.
func setsCDPATH(head string, words []string) bool {
	switch head {
	case "export", "declare", "typeset", "readonly", "local", "set":
		for _, w := range words[1:] {
			if w == "CDPATH" || strings.HasPrefix(w, "CDPATH=") {
				return true
			}
		}
	}
	return false
}

// isRunner reports a builtin that runs code able to cd the current shell.
func isRunner(prog string) bool {
	switch prog {
	case "eval", "source", ".":
		return true
	}
	return false
}

// applyCd moves the tracked directory for a bare cd, but only through forms
// the walk can resolve: a literal absolute path, a literal path under the
// home (no climbing above it, no ~user), a tracked `cd -`, and a literal
// relative path. Anything else leaves the directory unknown.
func applyCd(st *dirState, word string) {
	switch {
	case word == "":
		setDir(st, "~") // cd with no argument goes home
	case word == "-":
		if !st.prevOK {
			st.known = false
			return
		}
		st.dir, st.prev = st.prev, st.dir
	case isHomeLiteral(word):
		setDir(st, cleanHome(word))
	case strings.HasPrefix(word, "/") && !strings.ContainsAny(word, "$*?`"):
		setDir(st, path.Clean(word))
	case isLiteralRelative(word):
		j := joinDir(st.dir, word)
		if strings.HasPrefix(st.dir, "~") && !strings.HasPrefix(j, "~") {
			st.known = false // climbed out of the home directory
			return
		}
		setDir(st, j)
	default:
		st.known = false // a variable, glob or substitution: not resolvable
	}
}

func setDir(st *dirState, d string) {
	st.prev, st.dir, st.prevOK = st.dir, d, true
}

// isHomeLiteral reports ~, ~/literal, $HOME, $HOME/literal or ${HOME}/literal
// with no climbing (..) and no ~user.
func isHomeLiteral(word string) bool {
	rem, ok := homeRemainder(word)
	return ok && !strings.Contains(rem, "..")
}

// homeRemainder is what follows the home marker in a home-rooted word.
func homeRemainder(word string) (string, bool) {
	for _, pre := range []string{"${HOME}", "$HOME", "~"} {
		if word == pre {
			return "", true
		}
		if rest, ok := strings.CutPrefix(word, pre+"/"); ok {
			return "/" + rest, true
		}
	}
	return "", false
}

// cleanHome writes a home-rooted literal as ~/clean.
func cleanHome(word string) string {
	rem, _ := homeRemainder(word)
	if rem == "" {
		return "~"
	}
	return path.Clean("~" + rem)
}

// isLiteralRelative reports a relative path with no expansion and no home marker.
func isLiteralRelative(word string) bool {
	return word != "" && !strings.HasPrefix(word, "/") && !strings.HasPrefix(word, "~") &&
		!strings.ContainsAny(word, "$*?`")
}

// joinDir joins a relative path onto a base directory, treating a leading "~"
// as a literal first segment so the home marker is kept.
// joinDir joins a relative path onto a base directory, treating a leading "~"
// as a literal first segment so the home marker is kept.
func joinDir(base, rel string) string {
	rel = strings.TrimPrefix(rel, "./")
	if base == "" {
		return path.Clean(rel)
	}
	return path.Clean(path.Join(base, rel))
}

// wordText is a word with its quotes removed when it is literal throughout,
// and as written otherwise.
func wordText(src string, w *syntax.Word) string {
	var b strings.Builder
	for _, p := range w.Parts {
		switch x := p.(type) {
		case *syntax.Lit:
			b.WriteString(x.Value)
		case *syntax.SglQuoted:
			b.WriteString(x.Value)
		case *syntax.DblQuoted:
			for _, q := range x.Parts {
				l, ok := q.(*syntax.Lit)
				if !ok {
					return raw(src, w)
				}
				b.WriteString(l.Value)
			}
		default:
			return raw(src, w)
		}
	}
	return b.String()
}

func raw(src string, w *syntax.Word) string {
	start, end := int(w.Pos().Offset()), int(w.End().Offset())
	if start < 0 || end > len(src) || start >= end {
		return "$"
	}
	t := strings.Trim(src[start:end], `"'`)
	if !strings.HasPrefix(t, "$") && !strings.HasPrefix(t, "~") {
		t = "$" + t // never literal: it holds an expansion somewhere
	}
	return t
}

// wrappers run the program named after them.
var wrappers = map[string]bool{"sudo": true, "env": true, "command": true, "nohup": true, "time": true, "nice": true,
	"timeout": true, "exec": true, "builtin": true, "stdbuf": true, "ionice": true, "chrt": true, "doas": true}

// wrapperValueFlags are a wrapper's flags that take the next word as their value.
var wrapperValueFlags = map[string]bool{"-u": true, "-g": true, "-n": true, "-s": true, "-k": true, "-C": true,
	"-o": true, "-e": true, "-i": true, "-c": true, "-p": true, "-D": true, "--chdir": true}

// program is the program a command runs and the words after it.
func (c command) program() (string, []string) {
	w := c.words
	for len(w) > 0 {
		head := w[0]
		if strings.Contains(head, "=") && !strings.HasPrefix(head, "=") && !strings.HasPrefix(head, "$") {
			w = w[1:] // NAME=value before a program
			continue
		}
		if !wrappers[head] {
			return head, w[1:]
		}
		w = w[1:]
		if head == "timeout" {
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				w = w[1:]
			}
			if len(w) > 0 {
				w = w[1:] // the duration
			}
			continue
		}
		for len(w) > 0 && strings.HasPrefix(w[0], "-") {
			if wrapperValueFlags[w[0]] && len(w) > 1 {
				w = w[1:]
			}
			w = w[1:]
		}
	}
	return "", nil
}

// operands are the words that are not flags.
func operands(args []string) []string {
	var out []string
	end := false
	for _, a := range args {
		if !end && a == "--" {
			end = true
			continue
		}
		if !end && strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		out = append(out, a)
	}
	return out
}

func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f {
				return true
			}
		}
	}
	return false
}
