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
	// vars are shell variables set to a plain literal earlier in the same
	// plain chain, for resolving a $ target; nil where the directory is unknown.
	vars map[string]string
}

// script is what the rules read from a shell line.
type script struct {
	cmds []command
	// funcs are the functions the line defines; calling one is not running a program.
	funcs map[string]bool
	// setsPath is set when the line changes PATH, so which program runs is unknown.
	setsPath bool
	// mentionsHome is set when HOME appears anywhere in the line, which could
	// write it; ~ is then not placed against the session's home.
	mentionsHome bool
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
	s := script{funcs: map[string]bool{}, mentionsHome: strings.Contains(src, "HOME")}
	collect(src, f, &s)
	dirs := chainDirs(f, start)
	for i := range s.cmds {
		d, ok := dirs[s.cmds[i].call]
		s.cmds[i].dir, s.cmds[i].dirKnown = start, false
		if ok {
			s.cmds[i].dir, s.cmds[i].dirKnown, s.cmds[i].vars = d.dir, d.known, d.vars
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

// dirInfo is the directory a command in the chain runs in, whether it is
// known, and the literal variables set before it in the chain.
type dirInfo struct {
	dir   string
	known bool
	vars  map[string]string
}

// chainDirs claims a directory only where it is plain to see: the commands of
// the line's first statement when that statement is a single top-level &&
// chain (no ;, ||, &, newline, !, pipe, subshell, loop or condition), whose
// only directory changes are a plain `cd` to a plain literal path. Each
// command runs in the directory the cds before it left. A word that is not a
// single plain literal (a brace list, glob, backslash escape, quote or
// expansion) is never a cd operand or command name the check reads. After a
// shell builtin outside a short harmless set, a wrapper with an option, or a
// write to a directory variable, the directory is unknown for every command
// after it. Every command outside that first chain has an unknown directory.
func chainDirs(f *syntax.File, start string) map[*syntax.CallExpr]dirInfo {
	out := map[*syntax.CallExpr]dirInfo{}
	if len(f.Stmts) == 0 {
		return out
	}
	leaves, ok := andChain(f.Stmts[0])
	if !ok {
		return out
	}
	dir, known := start, true
	vars := map[string]string{}
	for _, leaf := range leaves {
		if call, ok := leaf.Cmd.(*syntax.CallExpr); ok {
			snap := map[string]string{}
			if known {
				for k, v := range vars {
					snap[k] = v
				}
			}
			out[call] = dirInfo{dir: dir, known: known, vars: snap}
			noteVars(call, vars)
		}
		// A leaf the check cannot read in full — any expansion in its words
		// or assignments, such as ${CDPATH:=/} — may write a variable cd or a
		// later target reads, so the directory and the literals end here.
		if !readLeaf(leaf) {
			known = false
			clear(vars)
		}
		if known {
			dir, known = stepDir(leaf, dir)
		}
	}
	return out
}

// noteVars records a bare assignment's plain literal values (X=/srv/old), and
// forgets a variable set to anything else.
func noteVars(call *syntax.CallExpr, vars map[string]string) {
	if len(call.Args) > 0 {
		return // X=1 cmd sets X for cmd only
	}
	for _, a := range call.Assigns {
		if a.Name == nil {
			continue
		}
		if a.Value != nil && a.Index == nil && a.Array == nil && !a.Append { // X+=… depends on X
			if v, ok := plainWord(a.Value); ok {
				vars[a.Name.Value] = v
				continue
			}
		}
		delete(vars, a.Name.Value)
	}
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

// bashBuiltins are the shell's own commands. A builtin can change the shell's
// state — its directory, a variable cd reads, how a name resolves — so after
// one the directory is unknown, unless it is in the harmless set.
var bashBuiltins = map[string]bool{}

// harmlessBuiltins change nothing a later cd or command name depends on.
var harmlessBuiltins = map[string]bool{"echo": true, "true": true, "false": true, ":": true, "test": true,
	"[": true, "pwd": true, "printf": true, "type": true, "times": true, "help": true}

func init() {
	for _, b := range strings.Fields(`. : [ alias bg bind break builtin caller cd command compgen complete compopt
		continue declare dirs disown echo enable eval exec exit export false fc fg getopts hash help history jobs
		kill let local logout mapfile popd printf pushd pwd read readarray readonly return set shift shopt source
		suspend test times trap true type typeset ulimit umask unalias unset wait`) {
		bashBuiltins[b] = true
	}
}

// stepDir is the directory after one leaf of the chain, and whether it is
// still known.
func stepDir(leaf *syntax.Stmt, dir string) (string, bool) {
	if decl, ok := leaf.Cmd.(*syntax.DeclClause); ok {
		if plainExport(decl) {
			return dir, true
		}
		return dir, false // declare, local, readonly, typeset, or export with options: may write by name
	}
	call, ok := leaf.Cmd.(*syntax.CallExpr)
	if !ok {
		return dir, false
	}
	for _, a := range call.Assigns {
		if a.Name != nil && dirVars[a.Name.Value] {
			return dir, false
		}
	}
	if len(call.Args) == 0 {
		return dir, true // an assignment only, and not to a directory variable
	}
	head, ok := plainWord(call.Args[0])
	if !ok {
		return dir, false // an escaped, quoted, globbed or expanded command name
	}
	base := path.Base(head)
	if head == "cd" {
		return plainCd(dir, call.Args[1:])
	}
	if base == "set" {
		if harmlessSet(call.Args[1:]) {
			return dir, true
		}
		return dir, false
	}
	if bashBuiltins[base] {
		if !harmlessBuiltins[base] || base == "printf" && printfSetsVar(call.Args[1:]) {
			return dir, false
		}
		return dir, true
	}
	if wrappers[base] {
		var words []string
		for _, w := range call.Args {
			v, ok := plainWord(w)
			if !ok {
				return dir, false // a wrapper's words the check cannot read plainly
			}
			words = append(words, v)
		}
		if optioned(wrapperPrefix(words)) {
			return dir, false // a wrapper with an option: what it runs, and where, is not plain
		}
		if prog, _ := (command{words: words}).program(); bashBuiltins[path.Base(prog)] {
			return dir, false // a builtin (cd among them) reached through a wrapper
		}
	}
	return dir, true // an external command does not move the shell
}

// printfSetsVar reports a printf that may write a variable by name: a plain
// -v, or any argument the check cannot read plainly ('-v', \-v, an expansion).
func printfSetsVar(args []*syntax.Word) bool {
	for _, w := range args {
		v, ok := plainWord(w)
		if !ok || v == "-v" || strings.HasPrefix(v, "-v") {
			return true
		}
	}
	return false
}

// readLeaf reports a chain leaf the check reads in full: every word and
// assignment value made of literal text only, with no expansion of any kind
// (parameter, command, arithmetic, process substitution) and no array or
// index assignment.
func readLeaf(leaf *syntax.Stmt) bool {
	var assigns []*syntax.Assign
	var words []*syntax.Word
	switch x := leaf.Cmd.(type) {
	case *syntax.CallExpr:
		assigns, words = x.Assigns, x.Args
	case *syntax.DeclClause:
		assigns = x.Args
	default:
		return false
	}
	for _, a := range assigns {
		if a.Index != nil || a.Array != nil {
			return false
		}
		if a.Value != nil {
			words = append(words, a.Value)
		}
	}
	for _, w := range words {
		if !literalText(w) {
			return false
		}
	}
	return true
}

// literalText reports a word with no expansion: literal, single- or
// double-quoted text only.
func literalText(w *syntax.Word) bool {
	for _, p := range w.Parts {
		switch x := p.(type) {
		case *syntax.Lit, *syntax.SglQuoted:
		case *syntax.DblQuoted:
			for _, q := range x.Parts {
				if _, ok := q.(*syntax.Lit); !ok {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

// plainExport reports `export NAME=literal …` with no options, each name
// outside the directory variables and each value a plain literal.
func plainExport(d *syntax.DeclClause) bool {
	if d.Variant == nil || d.Variant.Value != "export" || len(d.Args) == 0 {
		return false
	}
	for _, a := range d.Args {
		if a.Name == nil || dirVars[a.Name.Value] || a.Append || a.Index != nil || a.Array != nil {
			return false // an option (-n, -f) parses as a nameless assignment
		}
		if a.Value != nil {
			if _, ok := plainWord(a.Value); !ok {
				return false
			}
		}
	}
	return true
}

// safeSet are the set options that change nothing a cd or command name depends on.
var safeSet = map[string]bool{"pipefail": true, "errexit": true, "nounset": true, "xtrace": true}

// harmlessSet reports `set -e`, `-u`, `-x`, combined (`-eu`, `-eux`), and
// `-o pipefail|errexit|nounset|xtrace`, all plain; anything else (`set --`,
// positional parameters, other options) is not.
func harmlessSet(args []*syntax.Word) bool {
	if len(args) == 0 {
		return false
	}
	for _, w := range args {
		v, ok := plainWord(w)
		if !ok {
			return false
		}
		if safeSet[v] {
			continue
		}
		if !strings.HasPrefix(v, "-") || v == "-" || v == "--" {
			return false
		}
		for _, r := range v[1:] {
			if r != 'e' && r != 'u' && r != 'x' && r != 'o' {
				return false
			}
		}
	}
	return true
}

// plainWord is a word that is a single plain literal — letters, digits and
// / . _ + , @ % : = -, with a ~ allowed only first — or false for anything a
// shell would rewrite: a brace list, glob, backslash escape, quote, or
// expansion.
func plainWord(w *syntax.Word) (string, bool) {
	if w == nil || len(w.Parts) != 1 {
		return "", false
	}
	l, ok := w.Parts[0].(*syntax.Lit)
	if !ok || l.Value == "" {
		return "", false
	}
	for i, r := range l.Value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("/._+,@%:=-", r):
		case r == '~' && i == 0:
		default:
			return "", false
		}
	}
	return l.Value, true
}

// plainCd is the directory after `cd <args>`, known only for exactly one
// plain literal operand with no options: an absolute path, ~, ~/literal (no
// climbing above the home, no ~user), or a relative path.
func plainCd(dir string, args []*syntax.Word) (string, bool) {
	if len(args) != 1 {
		return dir, false // no argument, or more than one
	}
	w, ok := plainWord(args[0])
	if !ok || strings.HasPrefix(w, "-") {
		return dir, false // not a plain literal, an option, or cd -
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
		if !wrappers[path.Base(head)] {
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
		if !wrappers[path.Base(head)] {
			return head, w[1:]
		}
		w = w[1:]
		if path.Base(head) == "timeout" {
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

// plainProgram reports whether the program a command resolves to, past any
// wrappers, is a single plain literal word; a command with no call (a bare
// redirection) is plain.
func (c command) plainProgram() bool {
	prog, rest := c.program()
	if prog == "" || c.call == nil {
		return true
	}
	i := len(c.words) - len(rest) - 1
	if i < 0 || i >= len(c.call.Args) {
		return false
	}
	_, ok := plainWord(c.call.Args[i])
	return ok
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
