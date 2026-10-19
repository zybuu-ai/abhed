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
	// dir is the directory the command runs in; dirKnown says the rules may
	// rely on it. It is known only along a plain top-level && chain (see
	// chainDirs); everywhere else it is unknown.
	dir      string
	dirKnown bool
	// call is the command as parsed, to match it to its place in the line.
	call *syntax.CallExpr
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
// local workspace root, "~" the login's home over ssh). Every simple command
// is collected for the rules, in source order. The directory each runs in
// is worked out separately and claimed only where it is plain to see.
func parseScriptAt(src, start string) (script, bool) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil {
		return script{}, false
	}
	s := script{funcs: map[string]bool{}}
	collect(src, f, &s)
	dirs := chainDirs(src, f, start)
	for i := range s.cmds {
		d, ok := dirs[s.cmds[i].call]
		s.cmds[i].dir, s.cmds[i].dirKnown = start, false
		if ok {
			s.cmds[i].dir, s.cmds[i].dirKnown = d.dir, d.known
		}
	}
	return s, true
}

// collect gathers every simple command in the line, nested ones included.
func collect(src string, f *syntax.File, s *script) {
	syntax.Walk(f, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.FuncDecl:
			if x.Name != nil {
				s.funcs[x.Name.Value] = true
			}
		case *syntax.Assign:
			if x.Name != nil && x.Name.Value == "PATH" {
				s.setsPath = true
			}
		case *syntax.Stmt:
			call, ok := x.Cmd.(*syntax.CallExpr)
			if !ok {
				return true
			}
			c := command{call: call}
			for _, w := range call.Args {
				c.words = append(c.words, wordText(src, w))
			}
			for _, r := range x.Redirs {
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
			if len(c.words) > 0 || len(c.writes) > 0 || len(c.reads) > 0 {
				s.cmds = append(s.cmds, c)
			}
		}
		return true
	})
}

// dirInfo is the directory a command in the chain runs in, and whether it is known.
type dirInfo struct {
	dir   string
	known bool
}

// chainDirs claims a directory only where it is plain to see: the commands of
// the line's first statement when that statement is a single top-level &&
// chain (no ;, ||, &, newline, !, pipe, subshell, loop or condition), whose
// only directory changes are a plain `cd` to a literal path. Each command
// runs in the directory the cds before it in the chain left. Anything that
// could move the directory any other way, or could run code that might —
// a wrapper or runner with an option, eval, source, ., exec, trap, alias,
// pushd, popd, a write to HOME, OLDPWD or CDPATH, an expansion as a command
// name, `cd -`, a cd with options or to a value only running the line would
// give — leaves the directory unknown for every command after it. Every
// command outside that first chain has an unknown directory.
func chainDirs(src string, f *syntax.File, start string) map[*syntax.CallExpr]dirInfo {
	out := map[*syntax.CallExpr]dirInfo{}
	if len(f.Stmts) == 0 {
		return out
	}
	leaves, ok := andChain(f.Stmts[0])
	if !ok {
		return out
	}
	dir, known := start, true
	for _, leaf := range leaves {
		if call, ok := leaf.Cmd.(*syntax.CallExpr); ok {
			out[call] = dirInfo{dir: dir, known: known}
		}
		if known {
			dir, known = stepDir(src, leaf, dir)
		}
	}
	return out
}

// andChain is the leaf statements of a pure top-level && chain, in order, or
// false when the statement is anything else. A leaf is a simple command or a
// declaration (export, local and the like).
func andChain(st *syntax.Stmt) ([]*syntax.Stmt, bool) {
	if st.Negated || st.Background || st.Coprocess {
		return nil, false
	}
	switch x := st.Cmd.(type) {
	case *syntax.CallExpr, *syntax.DeclClause:
		return []*syntax.Stmt{st}, true
	case *syntax.BinaryCmd:
		if x.Op != syntax.AndStmt {
			return nil, false
		}
		l, ok := andChain(x.X)
		if !ok {
			return nil, false
		}
		r, ok := andChain(x.Y)
		if !ok {
			return nil, false
		}
		return append(l, r...), true
	}
	return nil, false
}

// dirVars are the variables whose writing changes where a later cd lands.
var dirVars = map[string]bool{"HOME": true, "OLDPWD": true, "CDPATH": true, "PWD": true}

// stepDir is the directory after one leaf of the chain, and whether it is
// still known.
func stepDir(src string, leaf *syntax.Stmt, dir string) (string, bool) {
	switch x := leaf.Cmd.(type) {
	case *syntax.DeclClause:
		for _, a := range x.Args {
			if a.Name != nil && dirVars[a.Name.Value] {
				return dir, false
			}
		}
		return dir, true
	case *syntax.CallExpr:
		for _, a := range x.Assigns {
			if a.Name != nil && dirVars[a.Name.Value] {
				return dir, false
			}
		}
		var words []string
		for _, w := range x.Args {
			words = append(words, wordText(src, w))
		}
		if len(words) == 0 {
			return dir, true // an assignment only, and not to a directory variable
		}
		head := words[0]
		if strings.HasPrefix(head, "$") || strings.HasPrefix(head, "~") {
			return dir, false // an expansion as the command name
		}
		switch head {
		case "eval", "source", ".", "exec", "trap", "alias", "unalias", "pushd", "popd":
			return dir, false
		case "cd":
			return plainCd(dir, words[1:])
		}
		if wrappers[head] {
			prefix := wrapperPrefix(words)
			if optioned(prefix) {
				return dir, false // a wrapper with an option: what it runs, and where, is not plain
			}
			if prog, _ := (command{words: words}).program(); prog == "cd" || prog == "pushd" || prog == "popd" {
				return dir, false // a cd through a wrapper
			}
		}
		return dir, true // an ordinary command does not move the shell
	}
	return dir, false
}

// plainCd is the directory after `cd <args>`, known only for exactly one
// literal operand with no options: an absolute path, ~, ~/literal (no
// climbing above the home, no ~user), or a literal relative path.
func plainCd(dir string, args []string) (string, bool) {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return dir, false // no argument, options, or cd -
	}
	w := args[0]
	if strings.ContainsAny(w, "$*?`") {
		return dir, false
	}
	switch {
	case strings.HasPrefix(w, "/"):
		return path.Clean(w), true
	case w == "~":
		return "~", true
	case strings.HasPrefix(w, "~/"):
		if strings.Contains(w, "..") {
			return dir, false
		}
		return path.Clean(w), true
	case strings.HasPrefix(w, "~"):
		return dir, false // ~user
	}
	j := joinDir(dir, w)
	if strings.HasPrefix(dir, "~") && !strings.HasPrefix(j, "~") {
		return dir, false // climbed out of the home
	}
	return j, true
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
		for len(w) > 0 && strings.HasPrefix(w[0], "-") {
			prefix = append(prefix, w[0])
			w = w[1:]
		}
	}
	return prefix
}

// optioned reports a wrapper prefix that carries any option, so what it runs
// and where cannot be read from the words alone.
func optioned(prefix []string) bool {
	for _, w := range prefix {
		if strings.HasPrefix(w, "-") {
			return true
		}
	}
	return false
}

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
