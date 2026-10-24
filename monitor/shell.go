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
	// stdin is text a here-string or here-document gives the command, which
	// an interpreter reads as its program.
	stdin []string
	// literal is set when every word, assignment value, redirection and
	// here-document body of the command is literal text, with no expansion.
	literal bool
	// assigns are the command's assignment values (the value of NAME=value).
	assigns []string
	// argsLiteral is set when every word of the command is literal text with
	// no expansion, substitution or glob, so its words are what will run.
	argsLiteral bool
	// env maps each NAME=value assignment before the command to its value.
	env map[string]string
	// printed is the literal an echo or printf command prints, if it is one.
	printed string
	// readsPipe is set when a pipe feeds this command and it reads its stdin
	// as code (a bare shell or interpreter), so the pipe's output is code.
	readsPipe bool
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
	// homeWrites is set when the line may write HOME, so ~ and $HOME are not
	// placed against the session's home. ~ depends on HOME, not the working
	// directory, so this is a line-level fact, not per command.
	homeWrites bool
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
	s.homeWrites = writesHomeText(src) || writesHomeStructural(f)
	scriptsWritten(&s)
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
	pipeReaders := map[*syntax.CallExpr]bool{}
	pipedText := map[*syntax.CallExpr]string{}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.BinaryCmd:
			if (x.Op == syntax.Pipe || x.Op == syntax.PipeAll) && x.Y != nil {
				if call, ok := x.Y.Cmd.(*syntax.CallExpr); ok && readsPipedCode(call) {
					pipeReaders[call] = true
					// When the left side only prints a literal, that text is the
					// code the reader runs; keep it so it can be judged. A cat with
					// no file reading a heredoc or here-string prints that body.
					if lx, ok := x.X.Cmd.(*syntax.CallExpr); ok {
						pipedText[call] = printedLiteral(src, lx)
						if pipedText[call] == "" {
							pipedText[call] = catBody(src, x.X, lx)
						}
					}
				}
			}
		case *syntax.FuncDecl:
			if x.Name != nil {
				s.funcs[x.Name.Value] = true
			}
		case *syntax.Assign:
			if x.Name != nil && x.Name.Value == "PATH" {
				s.setsPath = true
			}
		case *syntax.Stmt:
			if d, ok := x.Cmd.(*syntax.DeclClause); ok {
				// export/declare NAME=value: collect as a command so its
				// assignments (e.g. a git command-env variable) can be judged.
				var c command
				c.words = append(c.words, d.Variant.Value)
				c.argsLiteral = true
				c.env = map[string]string{}
				for _, a := range d.Args {
					if a.Name == nil {
						continue
					}
					v := ""
					if a.Value != nil {
						v = wordText(src, a.Value)
						if !literalText(a.Value) {
							c.argsLiteral = false
						}
					}
					c.words = append(c.words, a.Name.Value+"="+v)
					c.env[a.Name.Value] = v
				}
				if len(c.words) > 1 {
					s.cmds = append(s.cmds, c)
				}
				return true
			}
			call, ok := x.Cmd.(*syntax.CallExpr)
			if !ok {
				return true
			}
			c := command{call: call, literal: readLeaf(x), readsPipe: pipeReaders[call]}
			if t := pipedText[call]; t != "" {
				c.stdin = append(c.stdin, t)
			}
			c.argsLiteral = true
			for _, w := range call.Args {
				c.words = append(c.words, wordText(src, w))
				if !literalText(w) || globbed(w) {
					c.argsLiteral = false
				}
			}
			for _, a := range call.Assigns {
				if a.Value != nil {
					c.assigns = append(c.assigns, wordText(src, a.Value))
				}
				if a.Name != nil {
					if c.env == nil {
						c.env = map[string]string{}
					}
					v := ""
					if a.Value != nil {
						v = wordText(src, a.Value)
					}
					c.env[a.Name.Value] = v
				}
			}
			c.printed = printedLiteral(src, call)
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
				case syntax.WordHdoc:
					if r.Word != nil {
						c.stdin = append(c.stdin, wordText(src, r.Word))
					}
				case syntax.Hdoc, syntax.DashHdoc:
					if r.Hdoc != nil {
						c.stdin = append(c.stdin, wordText(src, r.Hdoc))
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
	for _, r := range leaf.Redirs {
		// A redirection's word and a here-document's body are expanded too:
		// `: <<< ${CDPATH:=/}` writes CDPATH as surely as an argument would.
		if r.Word != nil {
			words = append(words, r.Word)
		}
		if r.Hdoc != nil {
			words = append(words, r.Hdoc)
		}
	}
	for _, w := range words {
		if !literalText(w) {
			return false
		}
	}
	return true
}

// globbed reports a word with an unquoted glob character, whose value only
// running the line would give.
func globbed(w *syntax.Word) bool {
	for _, p := range w.Parts {
		if l, ok := p.(*syntax.Lit); ok && strings.ContainsAny(l.Value, "*?[") {
			return true
		}
	}
	return false
}

// scriptsWritten finds a script the line writes with echo or printf and then
// runs with a shell or interpreter, and gives the runner that text as code to
// judge, as if it were read on stdin.
func scriptsWritten(s *script) {
	written := map[string]string{}
	for i := range s.cmds {
		c := &s.cmds[i]
		prog, args := c.program()
		base := path.Base(prog)
		if readsStdinCode(base) {
			for _, a := range operands(args) {
				if t, ok := written[path.Clean(a)]; ok {
					c.stdin = append(c.stdin, t)
				}
				break // only the script operand
			}
		}
		if c.printed != "" {
			for _, w := range c.writes {
				written[path.Clean(w)] = c.printed
			}
		}
	}
}

// printedLiteral is the literal text an echo or printf prints, so a pipe into
// a shell can be judged on what it actually runs; "" when the producer is
// anything else or its arguments are not plain literals.
func printedLiteral(src string, call *syntax.CallExpr) string {
	var words []string
	for _, w := range call.Args {
		words = append(words, wordText(src, w))
	}
	prog, rest := (command{words: words}).program()
	switch path.Base(prog) {
	case "echo":
		var out []string
		for _, w := range rest {
			if strings.HasPrefix(w, "-") {
				continue // echo flags (-n, -e)
			}
			out = append(out, w)
		}
		return strings.Join(out, " ")
	case "printf":
		if len(rest) > 0 {
			return rest[0] // the format string
		}
	}
	return ""
}

// catBody is the heredoc or here-string body a bare cat prints, or "".
func catBody(src string, st *syntax.Stmt, call *syntax.CallExpr) string {
	if len(call.Args) != 1 {
		return "" // cat given a file reads that, not stdin
	}
	if v, ok := plainWord(call.Args[0]); !ok || path.Base(v) != "cat" {
		return ""
	}
	for _, r := range st.Redirs {
		switch {
		case r.Hdoc != nil:
			return wordText(src, r.Hdoc)
		case r.Op == syntax.WordHdoc && r.Word != nil:
			return wordText(src, r.Word)
		}
	}
	return ""
}

// readsPipedCode reports a pipe's reader that runs what it is piped as code:
// a bare shell or interpreter with no script or -c of its own.
func readsPipedCode(call *syntax.CallExpr) bool {
	var words []string
	for _, w := range call.Args {
		v, ok := plainWord(w)
		if !ok {
			return false
		}
		words = append(words, v)
	}
	prog, rest := (command{words: words}).program()
	base := path.Base(prog)
	if sqlClient[base] {
		return true // a SQL client reads its piped stdin as a query
	}
	// A container exec/run whose command is a SQL client passes the pipe on.
	if base == "docker" || base == "podman" || base == "nerdctl" || base == "kubectl" {
		for _, w := range rest {
			if sqlClient[path.Base(w)] {
				return true
			}
		}
	}
	if !readsStdinCode(base) {
		return false
	}
	// A reader given its own -c or a script operand is not reading the pipe as code.
	if _, hasC := shellDashC(rest); hasC {
		return false
	}
	return oneLiner(base, rest) == "" && firstOperandIsFile(rest)
}

// firstOperandIsFile reports a reader with no operand, or a stdin operand, so
// it takes its code from stdin rather than a named script.
func firstOperandIsFile(args []string) bool {
	for _, a := range operands(args) {
		return a == "-" || a == "/dev/stdin"
	}
	return true // no operand: reads stdin
}

// homeWriterBuiltins write a variable by name, so they may write HOME.
var homeWriterBuiltins = map[string]bool{"read": true, "mapfile": true, "readarray": true, "getopts": true,
	"eval": true, "source": true, ".": true}

// declMayWriteHome reports an export/declare/local/readonly/typeset that may
// write HOME: a name that is HOME, or a name the parser did not split out
// (declare "HO""ME=…"), which could be HOME.
func declMayWriteHome(d *syntax.DeclClause) bool {
	for _, a := range d.Args {
		switch {
		case a.Name != nil && a.Name.Value == "HOME":
			return true
		case a.Name == nil && a.Value != nil:
			return true
		}
	}
	return false
}

// writesHomeStructural reports a line with a statement that may write HOME in
// a way the plain-text scan would miss: an assignment to HOME, an export or
// declare the check cannot read in full, printf -v, read, eval, source, or any
// simple command the check cannot read plainly.
func writesHomeStructural(f *syntax.File) bool {
	found := false
	syntax.Walk(f, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.DeclClause:
			if declMayWriteHome(x) {
				found = true
			}
		case *syntax.CallExpr:
			for _, a := range x.Assigns {
				if a.Name != nil && a.Name.Value == "HOME" {
					found = true
				}
			}
			if len(x.Args) == 0 {
				return !found
			}
			head, ok := plainWord(x.Args[0])
			base := path.Base(head)
			switch {
			case !ok:
				found = true // an expanded or quoted command name
			case homeWriterBuiltins[base]:
				found = true
			case base == "printf" && printfSetsVar(x.Args[1:]):
				found = true
			}
		}
		return !found
	})
	return found
}

// writesHomeText reports a line that mentions HOME other than to read it as a
// plain $HOME or ${HOME}; such a line may write HOME, so ~ is not placed. It
// is a guard beside the chain walk, which already loses the directory at any
// HOME writer.
func writesHomeText(src string) bool {
	rest := strings.NewReplacer("${HOME}", "", "$HOME", "").Replace(src)
	return strings.Contains(rest, "HOME")
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

// harmlessSet reports `set` with only -e, -u, -x (alone or clustered) and
// `-o pipefail|errexit|nounset|xtrace`, all plain. An `o` in a cluster must be
// its last letter and be followed by one of those names; `set -o` alone,
// `set --`, positional parameters and any other option are not harmless.
func harmlessSet(args []*syntax.Word) bool {
	if len(args) == 0 {
		return false
	}
	for i := 0; i < len(args); i++ {
		v, ok := plainWord(args[i])
		if !ok || !strings.HasPrefix(v, "-") || v == "-" || v == "--" {
			return false
		}
		letters := v[1:]
		for k, r := range letters {
			if r == 'o' {
				if k != len(letters)-1 || i+1 >= len(args) {
					return false
				}
				name, ok := plainWord(args[i+1])
				if !ok || !safeSet[name] {
					return false
				}
				i++
				break
			}
			if r != 'e' && r != 'u' && r != 'x' {
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
			b.WriteString(unescape(x.Value, false))
		case *syntax.SglQuoted:
			b.WriteString(x.Value)
		case *syntax.DblQuoted:
			for _, q := range x.Parts {
				l, ok := q.(*syntax.Lit)
				if !ok {
					return raw(src, w)
				}
				b.WriteString(unescape(l.Value, true))
			}
		default:
			return raw(src, w)
		}
	}
	return b.String()
}

// unescape removes backslashes as bash does: outside quotes a backslash
// escapes any character (and a backslash-newline joins lines); inside double
// quotes only before $, `, ", \ and newline.
func unescape(v string, dquoted bool) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			n := v[i+1]
			if !dquoted || strings.IndexByte("$`\"\\\n", n) >= 0 {
				if n != '\n' {
					b.WriteByte(n)
				}
				i++
				continue
			}
		}
		b.WriteByte(v[i])
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
