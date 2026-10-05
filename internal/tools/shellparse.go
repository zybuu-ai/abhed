package tools

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// shellFacts is what a bash parser finds in a command, read beside the text
// checks: where the two disagree, the stricter one holds.
type shellFacts struct {
	// unparsed reports a command the parser refused, such as an unclosed
	// quote; nothing about its words can be trusted.
	unparsed bool
	// opaque says why the command runs text no rule can read, such as a
	// shell reading its commands from a pipe, or "" when it does not.
	opaque string
	// posix reports a way a prefix assignment to read can persist: POSIX mode,
	// or a function or alias named read.
	posix bool
	// fed marks the commands whose input is a pipe or a redirection made
	// around them: the reading end of a pipe, and every command inside a
	// subshell, group or block fed so, however deep.
	fed map[*syntax.Stmt]bool
	// top and nested say where the parser failed: on the text as given, or
	// on text it runs, such as eval's.
	top, nested bool
	// gitExtOn reports text that enables git's ext protocol.
	gitExtOn bool
	// gitBuilt says why a git command's arguments are made when it runs, so
	// the check cannot tell whether it discards work; "" when none are.
	gitBuilt string
	// incomplete reports that the text ended before a construct it opened
	// did: an unclosed quote, a trailing | or &&, an open loop or heredoc.
	incomplete bool
	// misplaced reports an error no earlier or later line can fix: a ! inside a pipeline.
	misplaced bool
	// execIn reports an exec that gave the shell itself new input.
	execIn bool
	// tooLong reports a command past what the parser reads; like the rules'
	// own split, that asks rather than refuses.
	tooLong bool
	// edited reports a program named by an expansion with an operator.
	edited bool
	// expands reports a program named by any other expansion that may split.
	expands bool
	// seps holds the separators literal IFS assignments set; ifsUnknown
	// reports IFS set in a way the text does not show.
	seps       string
	ifsUnknown bool
	// commands are the simple commands whose words are all literal, decoded,
	// from each word that may be the program: function bodies, subshells,
	// and the text eval, trap and a shell's -c run included.
	commands []string
	// src is the text being read, and gits where "git" appears in it, so a
	// substitution is found to name git without printing it again.
	src  string
	gits []int
	// bytes counts what commands hold; past maxCommandBytes the command is
	// taken as too long to read.
	bytes int
	// prefixes are assignments before read, which set how read splits its
	// input and not the shell's words.
	prefixes map[*syntax.Assign]bool
}

// maxReadings bounds the commands read from one simple command's words, so a
// long run of runners costs linear time.
const maxReadings = 8

// maxShellBytes bounds what the parser reads; a longer command is taken as
// hiding its words, as the rules' own split takes one.
const maxShellBytes = 64 << 10

// maxCommandBytes bounds the text the commands found may hold, a few times
// what the parser reads; past it the command asks as one too long does.
const maxCommandBytes = 4 * maxShellBytes

// maxShellDepth bounds how deep eval, trap and -c text is read; past it the
// command is taken as hiding its words.
const maxShellDepth = 8

// parseShell reads command with a bash parser.
func parseShell(command string) shellFacts {
	var f shellFacts
	f.read(command, 0)
	return f
}

func (f *shellFacts) read(command string, depth int) {
	if f.tooLong || len(command) > maxShellBytes {
		f.tooLong = true
		return
	}
	if depth > maxShellDepth {
		f.unparsed, f.nested = true, true
		return
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		// Only the text as given may be finished by lines after it; text
		// it runs that cannot be read stays unread.
		f.unparsed = true
		if depth == 0 {
			f.top, f.incomplete = true, syntax.IsIncomplete(err)
			f.misplaced = strings.Contains(err.Error(), "can only be used in full statements")
			f.readPrefix(command)
		} else {
			f.nested = true
		}
		return
	}
	f.walk(file, command, depth)
}

// continuing are the words a line can start with when it goes on from a
// construct begun on an earlier line.
var continuing = map[string]bool{"do": true, "then": true, "else": true, "elif": true, "done": true, "fi": true, "esac": true, "}": true, ")": true, ";;": true}

// readPrefix reads what can be read of a command that does not parse: the
// statements before the error, from after any keyword that continues an
// earlier line, so a line such as 'curl' x; a[b] or do c\url x still has
// its commands read, though it stays unparsed.
func (f *shellFacts) readPrefix(command string) {
	text := command
	for {
		text = strings.TrimLeft(text, " \t;")
		word, rest, _ := strings.Cut(text, " ")
		if !continuing[strings.TrimRight(word, ";")] {
			break
		}
		text = rest
	}
	var stmts []*syntax.Stmt
	for st, err := range syntax.NewParser(syntax.Variant(syntax.LangBash)).StmtsSeq(strings.NewReader(text)) {
		if err != nil {
			break
		}
		stmts = append(stmts, st)
	}
	if len(stmts) > 0 {
		f.walk(&syntax.File{Stmts: stmts}, text, 0)
	}
}

// posixMode finds POSIX mode turned on, in which an assignment before a
// function, read among them, stays.
var posixMode = regexp.MustCompile(`set\s+(?:-[a-zA-Z]*\s+)*[-+]o\s+posix|--posix|POSIXLY_CORRECT=|shopt\s+-s\s+-o\s+posix`)

// walk reads a parsed command, file, whose text is command.
func (f *shellFacts) walk(file *syntax.File, command string, depth int) {
	src, gits := f.src, f.gits
	defer func() { f.src, f.gits = src, gits }()
	f.src, f.gits = command, nil
	folded := command
	if FoldsCommandNames() {
		folded = strings.ToLower(command)
	}
	for i := 0; ; {
		j := strings.Index(folded[i:], "git")
		if j < 0 {
			break
		}
		f.gits = append(f.gits, i+j)
		i += j + 1
	}
	if posixMode.MatchString(command) {
		f.posix = true
	}
	// Kept for text run inside this, which inherits the environment.
	f.gitExtOn = f.gitExtOn || gitExtOn.MatchString(command)
	syntax.Walk(file, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.FuncDecl:
			f.posix = f.posix || n.Name != nil && n.Name.Value == "read"
		case *syntax.CallExpr:
			for _, a := range n.Args {
				if v, _ := literal(a); strings.HasPrefix(v, "read=") {
					f.posix = true
				}
			}
		}
		return !f.posix
	})
	syntax.Walk(file, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
				f.feed(n.Y)
			}
		case *syntax.Stmt:
			// Visited before what it holds, so its input passes inward.
			if f.fed[n] || inputRedirect(n) {
				for _, in := range innerStmts(n.Cmd) {
					f.feed(in)
				}
			}
			if c, ok := n.Cmd.(*syntax.CallExpr); ok {
				if inputRedirect(n) && bareExec(c.Args) {
					f.execIn = true // exec < f: every later command reads f
				}
				f.call(c, n, depth)
			}
		case *syntax.Assign:
			f.assign(n)
			f.subscriptText(n, depth)
		case *syntax.DeclClause:
			f.decl(n)
		case *syntax.ForClause:
			if it, ok := n.Loop.(*syntax.WordIter); ok && it.Name != nil && it.Name.Value == "IFS" {
				f.ifsUnknown = true
			}
		case *syntax.CoprocClause:
			if n.Name != nil {
				f.name(n.Name)
			}
		case *syntax.BinaryArithm:
			if arithAssignOps[n.Op] {
				f.arithTarget(n.X)
			}
		case *syntax.UnaryArithm:
			if n.Op == syntax.Inc || n.Op == syntax.Dec {
				f.arithTarget(n.X)
			}
		case *syntax.ParamExp:
			if n.Exp != nil && (n.Exp.Op == syntax.AssignUnset || n.Exp.Op == syntax.AssignUnsetOrNull) &&
				(n.Excl || n.Param == nil || n.Param.Value == "IFS") {
				f.ifsUnknown = true
			}
		}
		return true
	})
}

// bareExec reports an exec with no command, also behind command or builtin.
func bareExec(args []*syntax.Word) bool {
	i := 0
	for i < len(args) {
		v, _ := literal(args[i])
		switch v {
		case "command", "builtin":
			i++
			for i < len(args) {
				if o, _ := literal(args[i]); !strings.HasPrefix(o, "-") || o == "-" {
					break
				}
				i++
			}
			continue
		case "exec":
			return i == len(args)-1
		}
		return false
	}
	return false
}

var arithAssignOps = map[syntax.BinAritOperator]bool{
	syntax.Assgn: true, syntax.AddAssgn: true, syntax.SubAssgn: true, syntax.MulAssgn: true,
	syntax.QuoAssgn: true, syntax.RemAssgn: true, syntax.AndAssgn: true, syntax.OrAssgn: true,
	syntax.XorAssgn: true, syntax.ShlAssgn: true, syntax.ShrAssgn: true,
}

// arithTarget reads what an arithmetic assignment sets: IFS, or a name that
// is not plain text, is IFS set to an unknown value.
func (f *shellFacts) arithTarget(x syntax.ArithmExpr) {
	for {
		p, ok := x.(*syntax.ParenArithm)
		if !ok {
			break
		}
		x = p.X
	}
	w, ok := x.(*syntax.Word)
	if !ok {
		f.ifsUnknown = true
		return
	}
	name, lit := literal(w)
	if !lit || strings.Contains(name, "IFS") {
		f.ifsUnknown = true
	}
}

// assign reads an assignment: a literal value of IFS is its separators; any
// other value, or an index, leaves it unknown.
func (f *shellFacts) assign(a *syntax.Assign) {
	if f.prefixes[a] {
		return
	}
	if a.Name == nil {
		if a.Value != nil {
			f.declWord(a.Value)
		}
		return
	}
	if a.Name.Value != "IFS" {
		return
	}
	if a.Naked {
		f.ifsUnknown = true
		return
	}
	value, lit := "", true
	if a.Value != nil {
		value, lit = literal(a.Value)
	}
	if !lit || a.Index != nil || a.Array != nil {
		f.ifsUnknown = true
		return
	}
	f.seps += value
}

// subscriptText reads a literal value with a substitution in it as the
// commands it runs: arithmetic on the variable expands it, as in
// x='a[$(curl x)]'; echo $((x)).
func (f *shellFacts) subscriptText(a *syntax.Assign, depth int) {
	if a.Value == nil {
		return
	}
	v, lit := literal(a.Value)
	if !lit || !strings.Contains(v, "$(") && !strings.Contains(v, "`") {
		return
	}
	// Data that does not parse as text, such as a lone backquote, is left
	// as data; each whole substitution in it is still read on its own.
	parses := func(text string) bool {
		_, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(text), "")
		return err == nil
	}
	if text := ": " + v; parses(text) {
		f.read(text, depth+1)
		return
	}
	for _, text := range substitutionSpans(v) {
		if parses(text) {
			f.read(text, depth+1)
		}
	}
}

// decl reads declare and its kin: -n names its target in the value.
func (f *shellFacts) decl(d *syntax.DeclClause) {
	nameref := d.Variant.Value == "nameref"
	for _, a := range d.Args {
		if a.Naked && a.Name == nil && a.Value != nil {
			if flag, lit := literal(a.Value); lit && strings.HasPrefix(flag, "-") {
				nameref = nameref || strings.Contains(flag, "n")
			}
		}
	}
	if !nameref {
		return
	}
	for _, a := range d.Args {
		if a.Value != nil && !a.Naked {
			if v, lit := literal(a.Value); !lit || strings.Contains(v, "IFS") {
				f.ifsUnknown = true
			}
		}
	}
}

// declWord reads a declare argument the parser kept as a word, such as
// "${v}FS=_": a name built from an expansion, or one spelled IFS, may set it.
func (f *shellFacts) declWord(w *syntax.Word) {
	v, lit := literal(w)
	if !lit || !strings.HasPrefix(v, "-") && strings.Contains(v, "IFS") {
		f.ifsUnknown = true
	}
}

// name reads a word a command sets as a variable.
func (f *shellFacts) name(w *syntax.Word) {
	if v, lit := literal(w); !lit || strings.Contains(v, "IFS") {
		f.ifsUnknown = true
	}
}

// call reads one simple command: the words that may be its program, the
// variables it sets by name, and the shell text it runs.
func (f *shellFacts) call(c *syntax.CallExpr, st *syntax.Stmt, depth int) {
	words := make([]string, len(c.Args))
	lits := make([]bool, len(c.Args))
	for i, w := range c.Args {
		words[i], lits[i] = literal(w)
		if !lits[i] {
			words[i] = wordText(w)
		}
	}
	readings, dispatched := 0, false
	pos := position{on: true}
	for i, w := range c.Args {
		// Past the bound the command already asks; reading on only costs.
		if f.tooLong {
			return
		}
		value := pos.afterOption || pos.all
		if !pos.next(words[i]) {
			continue
		}
		// The command, and each later word that may be its program, are
		// commands for the rules; options, assignments and runners are not.
		// A word after an option may be its value or the program, so it is
		// read but not counted, nor is a duration; past maxReadings counted
		// the command asks as one too long does, rather than stop reading.
		reading := words[i]
		runner := readings > 0 && (gitRunners[CommandName(path.Base(reading))] || shellKeywords[reading])
		if reading != "" && reading[0] != '-' && !strings.Contains(reading, "=") && !runner {
			if !value && !isDuration(reading) {
				if readings++; readings > maxReadings {
					f.tooLong = true
				}
			}
			f.command(strings.Join(words[i:], " "))
		}
		if lits[i] && !value && !strings.Contains(reading, "=") && globWord(w) {
			f.expands = true // cur? and c*l name whatever file matches
		}
		if !lits[i] && !isAssignment(reading) {
			switch {
			case editedWord(w):
				f.edited = true
			case f.gitProgram(w):
				// Read as git by the git check and the rules, with the words after it.
				f.command(strings.Join(append([]string{"git"}, words[i+1:]...), " "))
				f.gitCall(c.Args[i+1:], depth)
			case splitsAsProgram(w):
				f.expands = true
			}
		}
		if !lits[i] {
			continue
		}
		if !dispatched {
			dispatched = f.runs(c, st, path.Base(words[i]), c.Args[i+1:], depth)
		}
	}
}

// runs reads what the command name does with its arguments, when it runs
// shell text or sets variables by name, and reports whether it was one.
func (f *shellFacts) runs(c *syntax.CallExpr, st *syntax.Stmt, name string, args []*syntax.Word, depth int) bool {
	switch {
	case name == "alias":
		// An alias's value is shell text run where its name is: read it so.
		for _, a := range args {
			v, lit := literal(a)
			if !lit {
				f.opaque = "an alias whose value is built from an expansion"
				continue
			}
			if _, value, ok := strings.Cut(v, "="); ok && !strings.HasPrefix(v, "-") {
				f.read(value, depth+1)
			}
		}
	case name == "hash" || name == "enable":
		// hash -p names a file to run for a name; enable -f loads a builtin.
		for _, a := range args {
			if v, _ := literal(a); strings.HasPrefix(v, "-") && strings.ContainsAny(v, map[string]string{"hash": "p", "enable": "f"}[name]) {
				f.opaque = name + " " + v + ", which makes a name run another program"
			}
		}
	case name == "source" || name == ".":
		if len(args) > 0 {
			if v, _ := literal(args[0]); stdinFile(v) {
				f.opaque = "a script read from its input"
			}
			if hasProcSubst(args[0]) {
				f.opaque = "a script made when it runs"
			}
		}
	case name == "eval":
		f.shellText(args, depth)
	case name == "trap":
		// trap's first argument is shell text run later; -p and -l list.
		for len(args) > 0 {
			if v, lit := literal(args[0]); lit && (v == "--" || v == "-p" || v == "-l") {
				args = args[1:]
				continue
			}
			break
		}
		if len(args) > 1 {
			f.shellText(args[:1], depth)
		}
	case name == "env":
		f.envSplit(args, depth)
		return false
	case name == "find":
		f.findExec(args)
		return false
	case name == "watch":
		f.watch(args, depth)
	case strings.TrimSuffix(CommandName(name), ".exe") == "git":
		f.gitCall(args, depth)
		return false
	case name == "su" || name == "flock" || name == "script":
		f.runner(args, depth)
	case cShells[name]:
		f.shell(st, args, depth)
	case ifsSetters[name]:
		if name == "read" && !f.posix {
			// A prefix assignment to the read builtin sets how read splits its
			// input only; in POSIX mode one before a function named read stays.
			if f.prefixes == nil {
				f.prefixes = map[*syntax.Assign]bool{}
			}
			for _, a := range c.Assigns {
				f.prefixes[a] = true
			}
		}
		f.setter(name, args)
	default:
		return false
	}
	return true
}

// shell reads what a shell runs: its -c text, or the commands it reads from
// its input when it is given -s or no script, which is read as text when the
// input is a literal here-string or heredoc, and is unknown when it is a pipe
// or a file, here or around it.
func (f *shellFacts) shell(st *syntax.Stmt, args []*syntax.Word, depth int) {
	stdin, script := false, false
scan:
	for j := 0; j < len(args); j++ {
		v, lit := literal(args[j])
		switch {
		case !lit:
			if hasProcSubst(args[j]) {
				f.opaque = "a shell running a script made when it runs"
			}
			script = true
			break scan
		case v == "--":
			script = j+1 < len(args)
			if script {
				if w, _ := literal(args[j+1]); stdinFile(w) {
					stdin = true
				}
			}
			break scan
		case v == "--rcfile" || v == "--init-file":
			j++ // its value is a file read at start, not the script
		case strings.HasPrefix(v, "--"):
		case len(v) > 1 && (v[0] == '-' || v[0] == '+'):
			value := false
			for _, o := range v[1:] {
				switch o {
				case 'c':
					if v[0] == '-' {
						if j+1 < len(args) {
							f.shellText(args[j+1:j+2], depth)
						}
						return
					}
				case 's':
					stdin = stdin || v[0] == '-'
				case 'o', 'O':
					value = true
				}
			}
			if value {
				j++ // set -o posix, -O extglob: the option's name
			}
		default:
			script = true
			stdin = stdin || stdinFile(v) || v == "-"
			break scan
		}
	}
	if script && !stdin {
		return // a script file, read by nothing here
	}
	// bash applies redirects left to right, so the last one on fd 0 is the input.
	var in *syntax.Redirect
	for _, r := range st.Redirs {
		if stdinRedirect(r) {
			in = r
		}
	}
	switch {
	case in == nil:
		if f.fed[st] || f.execIn || stdin && script {
			f.opaque = "a shell reading its commands from its input"
		}
	case in.Op == syntax.WordHdoc:
		f.shellText([]*syntax.Word{in.Word}, depth)
	case in.Op == syntax.Hdoc || in.Op == syntax.DashHdoc:
		if in.Hdoc != nil {
			f.shellText([]*syntax.Word{in.Hdoc}, depth)
		}
	default:
		f.opaque = "a shell reading its commands from a file"
	}
}

// stdinRedirect reports whether r sets fd 0: an input redirect with no fd
// named, or any redirect that names fd 0.
func stdinRedirect(r *syntax.Redirect) bool {
	if r.N != nil {
		n, err := strconv.Atoi(r.N.Value)
		return err == nil && n == 0
	}
	switch r.Op {
	case syntax.RdrIn, syntax.RdrInOut, syntax.DplIn, syntax.WordHdoc, syntax.Hdoc, syntax.DashHdoc:
		return true
	}
	return false
}

// runner reads the command su, flock or script run with -c or --command.
func (f *shellFacts) runner(args []*syntax.Word, depth int) {
	for j, a := range args {
		v, lit := literal(a)
		switch {
		case !lit:
		case v == "--command":
			if j+1 < len(args) {
				f.shellText(args[j+1:j+2], depth)
			}
			return
		case strings.HasPrefix(v, "--command="):
			f.read(strings.TrimPrefix(v, "--command="), depth+1)
			return
		case len(v) > 1 && v[0] == '-' && v[1] != '-' && strings.Contains(v, "c"):
			if j+1 < len(args) {
				f.shellText(args[j+1:j+2], depth)
			}
			return
		}
		if !lit && strings.HasPrefix(wordText(a), "--command") {
			f.opaque = "a command built when it runs"
			return
		}
	}
}

// envSplit reads env -S and --split-string, which split their value into
// the command env runs: literal text is read as one, built text is unknown.
func (f *shellFacts) envSplit(args []*syntax.Word, depth int) {
	for j, a := range args {
		v, lit := literal(a)
		text, next := "", false
		switch {
		case !lit:
			if t := wordText(a); strings.HasPrefix(t, "-S") || strings.HasPrefix(t, "--split-string") {
				f.opaque = "env -S given text built when it runs"
			}
			continue
		case v == "--split-string" || v == "-S":
			next = true
		case strings.HasPrefix(v, "--split-string="):
			text = strings.TrimPrefix(v, "--split-string=")
		case len(v) > 2 && v[0] == '-' && v[1] != '-' && strings.Contains(v, "S"):
			text = v[strings.IndexByte(v, 'S')+1:]
			next = text == ""
		case !strings.HasPrefix(v, "-"):
			return // the command, read as the program
		default:
			continue
		}
		if next && j+1 < len(args) {
			f.shellText(args[j+1:j+2], depth)
		} else if text != "" {
			f.read(text, depth+1)
		}
	}
}

// findExec reads the commands find runs with -exec, -execdir, -ok and -okdir:
// a command for the rules, and a program named by an expansion.
func (f *shellFacts) findExec(args []*syntax.Word) {
	for j := 0; j < len(args); j++ {
		v, _ := literal(args[j])
		if v != "-exec" && v != "-execdir" && v != "-ok" && v != "-okdir" {
			continue
		}
		var words []string
		k := j + 1
		for ; k < len(args); k++ {
			w, lit := literal(args[k])
			if lit && (w == ";" || w == "+") {
				break
			}
			if !lit {
				w = wordText(args[k])
			}
			words = append(words, w)
		}
		if len(words) > 0 {
			f.command(strings.Join(words, " "))
			if _, lit := literal(args[j+1]); !lit {
				if editedWord(args[j+1]) {
					f.edited = true
				} else if splitsAsProgram(args[j+1]) {
					f.expands = true
				}
			}
		}
		j = k
	}
}

// watch reads what watch runs: its words, joined, as shell text.
func (f *shellFacts) watch(args []*syntax.Word, depth int) {
	for j := 0; j < len(args); j++ {
		v, lit := literal(args[j])
		switch {
		case !lit:
			f.shellText(args[j:], depth)
			return
		case v == "-n" || v == "--interval" || v == "-q" || v == "--equexit":
			j++
		case strings.HasPrefix(v, "-"):
		default:
			f.shellText(args[j:], depth)
			return
		}
	}
}

// gitRunsCommand are git's options whose value is a command git runs.
var gitRunsCommand = []string{"--exec", "--upload-pack", "--receive-pack"}

// gitExtEscapes undoes the escapes of an ext:: URL; %s, %S, %G and %V become words.
var gitExtEscapes = strings.NewReplacer("%%", "%", "% ", " ", "%s", "x", "%S", "x", "%G", "x", "%V", "x")

// gitExtOn finds text that enables git's ext protocol: protocol.ext.allow,
// protocol.allow, GIT_ALLOW_PROTOCOL, or config passed through the environment.
var gitExtOn = regexp.MustCompile(`(?i)protocol\.(ext\.)?allow|GIT_ALLOW_PROTOCOL|GIT_CONFIG_(PARAMETERS|COUNT|KEY)`)

// gitTextOnly are subcommands whose arguments are text, never a URL git fetches.
var gitTextOnly = map[string]bool{"commit": true, "grep": true, "log": true, "show": true, "shortlog": true, "tag": true, "notes": true}

// gitExt reads the command an ext:: URL runs, wherever it is given: as a
// remote, a -c value or an option's value. While the line enables the ext
// protocol, an argument built when it runs may become such a URL.
func (f *shellFacts) gitExt(args []*syntax.Word, depth int) {
	extOn := f.gitExtOn
	i := gitSubcommand(args)
	for j := 0; j+1 < i; j++ {
		// A -c value built when it runs may enable the protocol.
		if v, _ := literal(args[j]); v == "-c" || v == "--config-env" {
			_, lit := literal(args[j+1])
			extOn = extOn || !lit
		}
	}
	scan := args
	if i < len(args) {
		if sub, lit := literal(args[i]); lit && gitTextOnly[sub] {
			scan = args[:i]
		}
	}
	for j, a := range args {
		v, lit := literal(a)
		if !lit {
			v = wordText(a)
			if extOn {
				f.gitBuilt = "git with the ext protocol enabled, which runs the command a URL names"
				f.opaque = "git given an argument built when it runs while the ext protocol is enabled"
				continue
			}
		}
		k := strings.Index(strings.ToLower(v), "ext::")
		if k < 0 || j >= len(scan) {
			continue
		}
		f.gitBuilt = "git with an ext:: URL, which runs the command it names"
		if !lit {
			f.opaque = "git given an ext:: URL built when it runs"
			continue
		}
		f.read(gitExtEscapes.Replace(v[k+len("ext::"):]), depth+1)
	}
}

// gitSubcommand is the index of git's subcommand in args, past its global
// options, or len(args).
func gitSubcommand(args []*syntax.Word) int {
	i := 0
	for i < len(args) {
		v, lit := literal(args[i])
		if !lit || !strings.HasPrefix(v, "-") {
			break
		}
		if gitGlobalsWithValue[v] {
			i++
		}
		i++
	}
	return min(i, len(args))
}

// gitCall reads a git command's arguments with their quoting known: for a
// subcommand with a form that discards work, an argument with an expansion
// or an unquoted glob (git reset "$1", git reset --h*) leaves it unreadable;
// for one that writes --output, so does such an argument that could become
// an option. An expansion after a literal --name= is that option's value,
// and a quoted pattern (git tag -l 'v*') is git's, not the shell's.
func (f *shellFacts) gitCall(args []*syntax.Word, depth int) {
	// --exec, --upload-pack and --receive-pack run their value as a command.
	for j, a := range args {
		v, lit := literal(a)
		t := v
		if !lit {
			t = wordText(a)
		}
		for _, opt := range gitRunsCommand {
			value, glued := strings.CutPrefix(t, opt+"=")
			if t != opt && !glued {
				continue
			}
			f.gitBuilt = "git " + opt + ", which runs the command it is given"
			switch {
			case !lit:
				f.opaque = "git " + opt + " given a command built when it runs"
			case glued:
				f.read(value, depth+1)
			case j+1 < len(args):
				f.shellText(args[j+1:j+2], depth)
			}
		}
	}
	f.gitExt(args, depth)
	i := gitSubcommand(args)
	if i >= len(args) {
		return
	}
	sub, lit := literal(args[i])
	if !lit || !gitDiscarding[sub] && !gitOutput[sub] {
		return
	}
	for _, a := range args[i+1:] {
		if l, ok := a.Parts[0].(*syntax.Lit); ok && strings.HasPrefix(l.Value, "--") && strings.Contains(l.Value, "=") {
			continue
		}
		v, lit := literal(a)
		if lit && !globWord(a) {
			continue
		}
		_, leadLit := a.Parts[0].(*syntax.Lit)
		switch {
		case gitDiscarding[sub]:
			f.gitBuilt = "git " + sub + " with an argument made when it runs, which the check cannot read"
			return
		case !leadLit || strings.HasPrefix(v, "-") || strings.HasPrefix(wordText(a), "-"):
			f.gitBuilt = "git " + sub + " with an argument that may become --output when it runs"
			return
		}
	}
}

// substitutionSpans are the outermost $(…) in v whose parentheses close,
// each as a command that runs it; found in one pass, and none inside
// another, so reading them all costs linear time.
func substitutionSpans(v string) []string {
	var open, starts, ends []int
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '(':
			sub := i > 0 && v[i-1] == '$'
			if !sub {
				open = append(open, -1)
			} else {
				open = append(open, i-1)
			}
		case ')':
			if len(open) == 0 {
				continue
			}
			start := open[len(open)-1]
			open = open[:len(open)-1]
			if start < 0 {
				continue
			}
			for len(starts) > 0 && starts[len(starts)-1] > start {
				starts, ends = starts[:len(starts)-1], ends[:len(ends)-1]
			}
			starts, ends = append(starts, start), append(ends, i+1)
		}
	}
	out := make([]string, len(starts))
	for k := range starts {
		out[k] = ": " + v[starts[k]:ends[k]]
	}
	return out
}

// feed marks st, a command whose input is a pipe or a redirection.
func (f *shellFacts) feed(st *syntax.Stmt) {
	if f.fed == nil {
		f.fed = map[*syntax.Stmt]bool{}
	}
	f.fed[st] = true
}

// inputRedirect reports a command given its input by a redirection.
func inputRedirect(st *syntax.Stmt) bool {
	for _, r := range st.Redirs {
		if stdinRedirect(r) {
			return true
		}
	}
	return false
}

// innerStmts are the commands a compound command runs with its input.
func innerStmts(cmd syntax.Command) []*syntax.Stmt {
	switch c := cmd.(type) {
	case *syntax.Subshell:
		return c.Stmts
	case *syntax.Block:
		return c.Stmts
	case *syntax.BinaryCmd:
		return []*syntax.Stmt{c.X, c.Y}
	case *syntax.IfClause:
		var out []*syntax.Stmt
		for ; c != nil; c = c.Else {
			out = append(append(out, c.Cond...), c.Then...)
		}
		return out
	case *syntax.WhileClause:
		return append(append([]*syntax.Stmt{}, c.Cond...), c.Do...)
	case *syntax.ForClause:
		return c.Do
	case *syntax.CaseClause:
		var out []*syntax.Stmt
		for _, it := range c.Items {
			out = append(out, it.Stmts...)
		}
		return out
	case *syntax.TimeClause:
		if c.Stmt != nil {
			return []*syntax.Stmt{c.Stmt}
		}
	}
	return nil
}

// hasProcSubst reports a word holding <(…) or >(…).
func hasProcSubst(w *syntax.Word) bool {
	for _, p := range w.Parts {
		if _, ok := p.(*syntax.ProcSubst); ok {
			return true
		}
	}
	return false
}

// stdinFile reports a file name that is the input itself.
func stdinFile(name string) bool {
	switch name {
	case "/dev/stdin", "/dev/fd/0", "/proc/self/fd/0":
		return true
	}
	return false
}

// globWord reports a word with an unquoted *, ? or [ … ] that the shell may
// turn into names of files; a brace list is read as its words elsewhere.
func globWord(w *syntax.Word) bool {
	for _, part := range w.Parts {
		l, ok := part.(*syntax.Lit)
		if !ok || l.Value == "[" {
			continue
		}
		// A glob character escaped with a backslash is itself.
		open := false
		for i := 0; i < len(l.Value); i++ {
			switch l.Value[i] {
			case '\\':
				i++
			case '*', '?':
				return true
			case '[':
				open = true
			case ']':
				if open {
					return true
				}
			}
		}
	}
	return false
}

// command keeps one command the rules and checks read, failing closed past
// maxCommandBytes.
func (f *shellFacts) command(text string) {
	if f.tooLong {
		return
	}
	if f.bytes += len(text); f.bytes > maxCommandBytes {
		f.tooLong = true
		return
	}
	f.commands = append(f.commands, text)
}

// shellText reads words a command runs as shell text: literal text is read
// as a command in its own right, and text built from an expansion hides
// both its program and what it may set.
func (f *shellFacts) shellText(args []*syntax.Word, depth int) {
	var parts []string
	for _, a := range args {
		v, lit := literal(a)
		if !lit {
			f.expands, f.ifsUnknown = true, true
			return
		}
		parts = append(parts, v)
	}
	if len(parts) > 0 {
		f.read(strings.Join(parts, " "), depth+1)
	}
}

// setter reads the names a command that sets variables by name is given:
// an option's value that is a name, glued or the next word, or an operand.
func (f *shellFacts) setter(setter string, args []*syntax.Word) {
	names := map[string]string{"printf": "v", "wait": "p", "read": "a"}[setter]
	values := map[string]string{"read": "dinNptu", "mapfile": "dnOsuCc", "readarray": "dnOsuCc"}[setter]
	operands := setter != "printf" && setter != "wait" && setter != "let" &&
		setter != "for" && setter != "select" && setter != "coproc"
	value, name := false, false
	for _, a := range args {
		v, lit := literal(a)
		switch {
		case name:
			f.name(a)
			name = false
		case value:
			value = false
		case lit && strings.HasPrefix(v, "-") && v != "-" && v != "--":
			for j := 1; j < len(v); j++ {
				if strings.IndexByte(names, v[j]) >= 0 {
					if rest := v[j+1:]; rest != "" {
						if strings.Contains(rest, "IFS") {
							f.ifsUnknown = true
						}
					} else {
						name = true
					}
					break
				}
				if strings.IndexByte(values, v[j]) >= 0 {
					value = v[j+1:] == ""
					break
				}
			}
		case !lit && strings.HasPrefix(wordText(a), "-"):
			// An option whose glued value is built: -v"${v}FS".
			f.ifsUnknown = true
		case operands:
			if !lit {
				f.ifsUnknown = true
			} else if n, _, _ := strings.Cut(v, "="); n == "IFS" || strings.HasPrefix(n, "IFS[") || strings.HasSuffix(n, "+") && strings.TrimSuffix(n, "+") == "IFS" {
				f.ifsUnknown = true
			}
		}
	}
}

// editedWord reports a word holding a ${...} that is more than a name: an
// operator, a length, an indirection or an index.
func editedWord(w *syntax.Word) bool {
	edited := false
	syntax.Walk(w, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			return false // its own commands are read in their own right
		case *syntax.ParamExp:
			edited = edited || !n.Short && !simpleParam(n)
		}
		return !edited
	})
	return edited
}

func simpleParam(p *syntax.ParamExp) bool {
	return p.Param != nil && !p.Excl && !p.Length && !p.Width && p.Index == nil &&
		p.Slice == nil && p.Repl == nil && p.Names == 0 && p.Exp == nil && p.NestedParam == nil
}

// splitsAsProgram reports a program's word whose expansion may name the
// program or split into its words. Only a plain expansion of a named or
// numbered parameter, or a substitution, inside double quotes and followed by
// a /, is a directory and nothing more; "$@" is a word per parameter.
func splitsAsProgram(w *syntax.Word) bool {
	for i, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit, *syntax.SglQuoted:
		case *syntax.ParamExp:
			// $IFS is read as the blank it is by the canonical text and the IFS checks.
			if !simpleParam(p) || p.Param.Value != "IFS" {
				return true
			}
		case *syntax.DblQuoted:
			for j, in := range p.Parts {
				if _, ok := in.(*syntax.Lit); ok {
					continue
				}
				if !directoryExpansion(in) {
					return true
				}
				if !followedBySlash(p.Parts[j+1:], w.Parts[i+1:]) {
					return true
				}
			}
		default:
			return true
		}
	}
	return false
}

// gitProgram reports a program's word that is one substitution naming git,
// such as $(which git), which the git check reads as git. It looks the
// substitution's span up among where "git" appears, without reading it again.
func (f *shellFacts) gitProgram(w *syntax.Word) bool {
	if len(w.Parts) != 1 {
		return false
	}
	cs, ok := w.Parts[0].(*syntax.CmdSubst)
	if !ok {
		return false
	}
	start, end := int(cs.Left.Offset()), int(cs.Right.Offset())
	i := sort.SearchInts(f.gits, start)
	return i < len(f.gits) && f.gits[i]+3 <= end
}

// directoryExpansion reports an expansion inside double quotes that is one
// word: $name, ${name}, $1 or a command substitution.
func directoryExpansion(part syntax.WordPart) bool {
	switch p := part.(type) {
	case *syntax.CmdSubst:
		return true
	case *syntax.ParamExp:
		if !simpleParam(p) {
			return false
		}
		name := p.Param.Value
		return name != "" && name != "@" && name != "*" && (name[0] == '_' || isNameByte(name[0]))
	}
	return false
}

// followedBySlash reports a / right after an expansion: next in its quotes,
// or first after them when it ends them.
func followedBySlash(inner, after []syntax.WordPart) bool {
	if len(inner) > 0 {
		l, ok := inner[0].(*syntax.Lit)
		return ok && strings.HasPrefix(l.Value, "/")
	}
	if len(after) == 0 {
		return false
	}
	switch p := after[0].(type) {
	case *syntax.Lit:
		return strings.HasPrefix(p.Value, "/")
	case *syntax.DblQuoted:
		if len(p.Parts) > 0 {
			l, ok := p.Parts[0].(*syntax.Lit)
			return ok && strings.HasPrefix(l.Value, "/")
		}
	}
	return false
}

// literal is a word's value when it has no expansion, with its quoting undone.
func literal(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(unescape(p.Value, ""))
		case *syntax.SglQuoted:
			if p.Dollar {
				text, _ := ansiC(p.Value+"'", 0)
				b.WriteString(text)
			} else {
				b.WriteString(p.Value)
			}
		case *syntax.DblQuoted:
			for _, in := range p.Parts {
				l, ok := in.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(unescape(l.Value, "$`\"\\\n"))
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// unescape removes the backslashes the shell would: before any character
// unquoted, or before one of only in double quotes.
func unescape(s, only string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (only == "" || strings.IndexByte(only, s[i+1]) >= 0) {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// wordText is a word as written, with each substitution, edited expansion
// and arithmetic as one placeholder word: what is inside them is read on its
// own, and printing it into every word around it would cost the square of
// its nesting.
func wordText(w *syntax.Word) string {
	var b strings.Builder
	for _, p := range w.Parts {
		partText(&b, p)
	}
	return b.String()
}

func partText(b *strings.Builder, part syntax.WordPart) {
	switch p := part.(type) {
	case *syntax.Lit:
		b.WriteString(p.Value)
	case *syntax.SglQuoted:
		if p.Dollar {
			b.WriteByte('$')
		}
		b.WriteString("'" + p.Value + "'")
	case *syntax.DblQuoted:
		if p.Dollar {
			b.WriteByte('$')
		}
		b.WriteByte('"')
		for _, in := range p.Parts {
			partText(b, in)
		}
		b.WriteByte('"')
	case *syntax.ParamExp:
		if simpleParam(p) {
			b.WriteString("${" + p.Param.Value + "}")
		} else {
			b.WriteString("${EDITED}")
		}
	case *syntax.CmdSubst, *syntax.ProcSubst:
		b.WriteString(substitution)
	default:
		b.WriteString("$EXPANSION")
	}
}
