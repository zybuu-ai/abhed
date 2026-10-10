package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A shell fed its commands on stdin, while rules or hooks screen commands,
// is given them one statement at a time: lines are held until the parser
// reads a complete statement, which is judged whole, as an exec's command
// line is, and recorded; only then does the shell see it. A statement that
// is refused, or cannot be judged, ends the exec, and nothing after it is
// sent. Commands inside the script that would read the rest of stdin as
// something other than these statements (an interpreter, eval of text built
// when it runs, a file the script wrote) are refused as uncheckable.

const (
	// stdinStatementBytes bounds one statement, what the policy parser reads whole.
	stdinStatementBytes = 64 << 10
	// stdinScreenBytes bounds the stdin one exec may have judged.
	stdinScreenBytes = 8 << 20
	// stdinParseBytes bounds the text parsed again while statements span
	// many lines, so a long open statement costs bounded time.
	stdinParseBytes = 256 << 20
)

// stdinScreen is an exec's stdin, judged statement by statement once on.
type stdinScreen struct {
	s      *Server
	pol    *policy.Engine
	ctx    context.Context
	caller controlplane.Caller
	sb     controlplane.Sandbox
	src    io.Reader
	// on is set by the screen of the command line, before the command runs.
	on bool

	buf     []byte
	pending []byte
	scan    int
	out     []byte
	// err ends the input: io.EOF, the client's error, or a refusal, which
	// the shell sees as the end of its input, so it finishes what it was
	// given and nothing after.
	err     error
	refusal error
	read    int
	parsed  int
	n       int
	script  *stdinScript
}

func (s *Server) newStdinScreen(ctx context.Context, pol *policy.Engine, c controlplane.Caller, sb controlplane.Sandbox,
	src io.Reader) *stdinScreen {
	return &stdinScreen{s: s, pol: pol, ctx: ctx, caller: c, sb: sb, src: src, script: newStdinScript()}
}

// refused is why the input was cut short, nil when it was not; asked once
// the command has ended.
func (q *stdinScreen) refused() error { return q.refusal }

func (q *stdinScreen) Read(p []byte) (int, error) {
	if !q.on {
		return q.src.Read(p)
	}
	if q.buf == nil {
		q.buf = make([]byte, 32<<10)
	}
	for len(q.out) == 0 {
		if q.err != nil {
			return 0, q.err
		}
		if q.next() {
			continue
		}
		n, err := q.src.Read(q.buf)
		q.pending = append(q.pending, q.buf[:n]...)
		if q.read += n; q.read > stdinScreenBytes {
			q.refuseNext(fmt.Sprintf("the input is longer than the %d bytes that are judged", stdinScreenBytes))
			continue
		}
		switch {
		case errors.Is(err, io.EOF):
			for q.err == nil && q.next() {
			}
			if q.err == nil {
				q.last()
			}
			if q.err == nil {
				q.err = io.EOF
			}
		case err != nil:
			q.err = err
		}
	}
	n := copy(p, q.out)
	q.out = q.out[n:]
	return n, nil
}

// next judges the first complete statement held, reporting whether it found one.
func (q *stdinScreen) next() bool {
	for q.err == nil {
		i := bytes.IndexByte(q.pending[q.scan:], '\n')
		if i < 0 {
			if len(q.pending) > stdinStatementBytes {
				q.refuseNext(fmt.Sprintf("a statement is longer than %d bytes", stdinStatementBytes))
			}
			return false
		}
		end := q.scan + i + 1
		q.scan = end
		if end > stdinStatementBytes {
			q.refuseNext(fmt.Sprintf("a statement is longer than %d bytes", stdinStatementBytes))
			return false
		}
		text := q.pending[:end]
		if continued(text) {
			continue
		}
		if q.parsed += end; q.parsed > stdinParseBytes {
			q.refuseNext("the input's statements span too many lines to judge")
			return false
		}
		f, err := parseScript(text)
		if openStatement(err) {
			continue
		}
		q.judge(text, f, err)
		if q.err != nil {
			return false
		}
		q.out = append(q.out, text...)
		q.pending, q.scan = append([]byte(nil), q.pending[end:]...), 0
		return true
	}
	return false
}

// last judges what is held once the input has ended.
func (q *stdinScreen) last() {
	if len(bytes.TrimSpace(q.pending)) == 0 {
		q.out = append(q.out, q.pending...)
		q.pending = nil
		return
	}
	f, err := parseScript(q.pending)
	if openStatement(err) {
		q.refuseNext("the input ended inside a statement")
		return
	}
	q.judge(q.pending, f, err)
	if q.err == nil {
		q.out = append(q.out, q.pending...)
		q.pending = nil
	}
}

// continued reports a line ending in a backslash the shell joins to the next.
func continued(text []byte) bool {
	n := 0
	for i := len(text) - 2; i >= 0 && text[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

// openStatement reports a parse that more lines could finish; the parser
// does not count an open here-document as incomplete.
func openStatement(err error) bool {
	return err != nil && (syntax.IsIncomplete(err) || strings.Contains(err.Error(), "unclosed here-document"))
}

func parseScript(text []byte) (*syntax.File, error) {
	return syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(false)).Parse(bytes.NewReader(text), "")
}

// judge puts one statement to the rules, hooks and the uncheckable checks,
// records it, and refuses it when any of them does.
func (q *stdinScreen) judge(text []byte, f *syntax.File, perr error) {
	stmt := string(text)
	if perr == nil && len(f.Stmts) == 0 {
		return // blank lines and comments
	}
	q.n++
	why := ""
	switch {
	case perr != nil:
		why = "the statement cannot be parsed, so it cannot be checked"
	case bytes.IndexByte(text, 0) >= 0:
		why = "the statement holds a NUL byte, so it cannot be checked"
	}
	unread := ""
	if why == "" {
		why, unread = q.policy(stmt)
	}
	if why == "" {
		why = q.script.check(f)
	}
	// A program named by a variable the script set to known text is read in
	// the lines resolved with it; anything else no rule can read is refused,
	// as on an exec.
	resolved := unread != "" && strings.Contains(unread, "program named by an expansion") && len(q.script.resolved) > 0
	if why == "" {
		for _, line := range q.script.resolved {
			u := ""
			if why, u = q.policy(line); why != "" {
				break
			}
			resolved = resolved && u == ""
		}
	}
	if why == "" && unread != "" && !resolved {
		why = unread + ", which no one can confirm on a statement read from stdin"
	}
	if why == "" {
		if opaque := tools.CanonicalCommand(stmt).Opaque(); opaque != "" {
			why = "the statement runs text no rule can read (" + opaque + ")"
		}
	}
	if why != "" {
		q.refuse(stmt, why)
		return
	}
	detail := map[string]string{"statement": stmt, "n": strconv.Itoa(q.n)}
	if err := q.s.opts.ControlPlane.Note(context.WithoutCancel(q.ctx), q.caller, q.sb, "exec.input", "", detail, "statement"); err != nil {
		q.s.log.Warn("a statement read from stdin could not be recorded", "sandbox", q.sb.ID, "error", err)
	}
}

// policy is why the rules or hooks refuse a statement, "" when they do not.
// An ask rule's or a hook's question refuses it: no one is there to answer.
// unread is why the rules cannot read it, as an exec refuses it.
func (q *stdinScreen) policy(stmt string) (why, unread string) {
	args, _ := json.Marshal(map[string]string{"command": stmt})
	d := q.pol.Evaluate("bash", true, args)
	switch {
	case d.Decision == policy.Deny:
		return "denied by policy: " + d.Reason, ""
	case d.Decision == policy.Ask && (d.Step == "ask" || d.Step == "hook"):
		return "it needs approval (" + d.Reason + "), which no one can give to a statement read from stdin", ""
	}
	return "", unreadable(d, stmt)
}

// refuseNext refuses the statement held and not yet judged, which judge
// has not counted: it is the next one.
func (q *stdinScreen) refuseNext(why string) {
	q.n++
	q.refuse(string(q.pending), why)
}

// refuse ends the input at statement q.n, recording it and why.
func (q *stdinScreen) refuse(stmt, why string) {
	detail := map[string]string{"statement": stmt, "reason": why, "n": strconv.Itoa(q.n)}
	if err := q.s.opts.ControlPlane.Note(context.WithoutCancel(q.ctx), q.caller, q.sb, "exec.refused", controlplane.OutcomeFailed,
		detail, "statement", "reason"); err != nil {
		q.s.log.Warn("a refused statement read from stdin could not be recorded", "sandbox", q.sb.ID, "error", err)
	}
	q.refusal = fmt.Errorf("%w: statement %d read from stdin was refused and nothing after it ran: %s", controlplane.ErrForbidden, q.n, why)
	q.err = io.EOF
}

// stdinScript is what the statements judged so far have set up: the
// variables they set, and the files they wrote, by base name.
type stdinScript struct {
	// vars holds each variable a statement set, with its value when that is
	// known text and nil when it is not.
	vars    map[string]*string
	written map[string]bool
	// writtenUnknown reports a file written whose name is not known text.
	writtenUnknown bool
	ifs            bool
	// resolved are the lines found in a statement once variables it set
	// earlier are put in, for the rules to see too.
	resolved []string
}

func newStdinScript() *stdinScript {
	return &stdinScript{vars: map[string]*string{}, written: map[string]bool{}}
}

func (sc *stdinScript) clone() *stdinScript {
	c := &stdinScript{vars: maps.Clone(sc.vars), written: maps.Clone(sc.written), writtenUnknown: sc.writtenUnknown, ifs: sc.ifs}
	return c
}

// check is why a statement cannot be judged, "" when it can, and notes
// what it sets for the statements after it.
func (sc *stdinScript) check(f *syntax.File) string {
	sc.resolved = nil
	w := &scriptWalk{sc: sc}
	for _, st := range f.Stmts {
		w.stmt(st, true, false)
		if w.why != "" {
			break
		}
	}
	return w.why
}

// scriptWalk walks one statement. in says whether a command reads the
// shell's own input; cond whether it may not run, so what it sets is unknown.
type scriptWalk struct {
	sc  *stdinScript
	why string
}

func (w *scriptWalk) fail(why string) {
	if w.why == "" {
		w.why = why
	}
}

func (w *scriptWalk) stmts(list []*syntax.Stmt, in, cond bool) {
	for _, st := range list {
		w.stmt(st, in, cond)
	}
}

func (w *scriptWalk) stmt(st *syntax.Stmt, in, cond bool) {
	if w.why != "" || st == nil {
		return
	}
	outer := in
	cond = cond || st.Background || st.Coprocess
	var input *syntax.Redirect
	for _, r := range st.Redirs {
		w.words(outer, r.Word)
		if r.Hdoc != nil {
			w.words(outer, r.Hdoc)
		}
		if dupsInput(r) {
			w.fail("a copy of the shell's own input is made for a command, which reads statements no rule has seen")
			return
		}
		if stdinRedirect(r) {
			input = r
			in = false
			if v, ok := w.sc.literal(r.Word); ok && (stdinPath(v) || r.Op == syntax.DplIn && v == "0") {
				in = true
			}
			continue
		}
		if writesFile(r) {
			w.wrote(r.Word)
		}
	}
	if call, ok := st.Cmd.(*syntax.CallExpr); ok && input != nil && len(call.Args) > 0 {
		if v, ok := w.sc.literal(call.Args[0]); ok && programName(v) == "exec" {
			w.fail("exec gives the shell new input, which no rule sees")
			return
		}
	}
	w.cmd(st.Cmd, in, outer, cond, input)
}

// dupsInput reports a redirect copying fd 0 to another descriptor.
func dupsInput(r *syntax.Redirect) bool {
	if r.N == nil || r.N.Value == "0" || (r.Op != syntax.DplIn && r.Op != syntax.DplOut) || r.Word == nil {
		return false
	}
	v, lit := literalWord(r.Word)
	return !lit || v == "0" || v == "0-"
}

// stdinRedirect reports a redirect that sets fd 0.
func stdinRedirect(r *syntax.Redirect) bool {
	if r.N != nil {
		return r.N.Value == "0"
	}
	switch r.Op {
	case syntax.RdrIn, syntax.RdrInOut, syntax.DplIn, syntax.WordHdoc, syntax.Hdoc, syntax.DashHdoc:
		return true
	}
	return false
}

// writesFile reports a redirect that writes a file.
func writesFile(r *syntax.Redirect) bool {
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.RdrAll, syntax.AppAll, syntax.RdrInOut:
		return true
	}
	return false
}

// stdinPath reports a file name that is the shell's input itself.
func stdinPath(v string) bool {
	return v == "-" || v == "/dev/stdin" || v == "/dev/fd/0" || strings.HasPrefix(v, "/proc/") && strings.HasSuffix(v, "/fd/0")
}

func (w *scriptWalk) cmd(c syntax.Command, in, outer, cond bool, input *syntax.Redirect) {
	if w.why != "" || c == nil {
		return
	}
	switch c := c.(type) {
	case *syntax.CallExpr:
		w.call(c, in, outer, cond, input)
	case *syntax.BinaryCmd:
		switch c.Op {
		case syntax.Pipe, syntax.PipeAll:
			w.stmt(c.X, in, true)
			w.stmt(c.Y, false, true)
		default:
			w.stmt(c.X, in, cond)
			w.stmt(c.Y, in, true)
		}
	case *syntax.Subshell:
		w.stmts(c.Stmts, in, true)
	case *syntax.Block:
		w.stmts(c.Stmts, in, cond)
	case *syntax.IfClause:
		for ; c != nil; c = c.Else {
			w.stmts(c.Cond, in, true)
			w.stmts(c.Then, in, true)
		}
	case *syntax.WhileClause:
		w.stmts(c.Cond, in, true)
		w.stmts(c.Do, in, true)
	case *syntax.ForClause:
		if it, ok := c.Loop.(*syntax.WordIter); ok {
			w.set(it.Name.Value, nil)
			w.words(outer, it.Items...)
		} else {
			w.nodes(outer, c.Loop)
		}
		w.stmts(c.Do, in, true)
	case *syntax.CaseClause:
		w.words(outer, c.Word)
		for _, item := range c.Items {
			w.words(outer, item.Patterns...)
			w.stmts(item.Stmts, in, true)
		}
	case *syntax.FuncDecl:
		// Run later, wherever it is called: read as reading the shell's input.
		w.stmt(c.Body, true, true)
	case *syntax.DeclClause:
		if c.Variant.Value == "nameref" {
			w.fail("a name reference, which makes one variable stand for another")
			return
		}
		for _, a := range c.Args {
			if a.Naked && a.Name == nil {
				v, ok := w.sc.literal(a.Value)
				switch {
				case !ok:
					w.fail(c.Variant.Value + " of a variable named when it runs")
				case strings.HasPrefix(v, "-") && strings.Contains(v, "n"):
					w.fail("a name reference, which makes one variable stand for another")
				}
				continue
			}
			w.assign(a, outer, cond || c.Variant.Value == "local")
		}
	case *syntax.TimeClause:
		w.stmt(c.Stmt, in, cond)
	case *syntax.CoprocClause:
		w.stmt(c.Stmt, in, true)
	case *syntax.LetClause, *syntax.ArithmCmd, *syntax.TestClause:
		w.nodes(outer, c)
	default:
		w.fail("a statement of a kind that cannot be checked")
	}
}

func (w *scriptWalk) assign(a *syntax.Assign, outer, cond bool) {
	w.nodes(outer, a)
	if a.Name == nil || a.Naked {
		return // export NAME keeps its value
	}
	if a.Value == nil || a.Append || a.Array != nil || a.Index != nil || cond {
		w.set(a.Name.Value, nil)
		return
	}
	// An assignment's value is not split, so what is unknown in it stays one word.
	v := strings.ReplaceAll(w.sc.text(a.Value), splitUnknown, quietUnknown)
	w.set(a.Name.Value, &v)
}

// set notes a variable the script set; one a shell runs or reads as
// commands on its own is refused.
func (w *scriptWalk) set(name string, v *string) {
	if shellStartFiles[name] {
		w.fail(name + " is set, which a shell reads as commands or files to run on its own")
		return
	}
	if name == "IFS" {
		w.sc.ifs = true
	}
	w.sc.vars[name] = v
}

// words walks the substitutions in words, which run with the input the
// statement has before its own redirects.
func (w *scriptWalk) words(in bool, words ...*syntax.Word) {
	for _, wd := range words {
		if wd != nil {
			w.nodes(in, wd)
		}
	}
}

func (w *scriptWalk) nodes(in bool, n syntax.Node) {
	syntax.Walk(n, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.CmdSubst:
			w.stmts(n.Stmts, in, true)
			return false
		case *syntax.ProcSubst:
			w.stmts(n.Stmts, in, true)
			return false
		}
		return w.why == ""
	})
}

// wrote notes a file a command writes, by base name.
func (w *scriptWalk) wrote(word *syntax.Word) {
	if word == nil {
		return
	}
	if base, ok := w.sc.base(word); ok && base != "" {
		w.sc.written[base] = true
	} else {
		w.sc.writtenUnknown = true
	}
}

// call judges one simple command.
func (w *scriptWalk) call(c *syntax.CallExpr, in, outer, cond bool, input *syntax.Redirect) {
	for _, a := range c.Assigns {
		if len(c.Args) == 0 {
			w.assign(a, outer, cond)
		} else {
			w.nodes(outer, a)
			if shellStartFiles[a.Name.Value] {
				w.fail(a.Name.Value + " is set, which a shell reads as commands or files to run on its own")
			}
		}
	}
	w.words(outer, c.Args...)
	if w.why != "" || len(c.Args) == 0 {
		return
	}
	texts := make([]string, len(c.Args))
	lits := make([]bool, len(c.Args))
	changed := false
	for i, a := range c.Args {
		texts[i], lits[i] = w.sc.literal(a)
		if lits[i] {
			if plain, ok := literalWord(a); !ok || plain != texts[i] {
				changed = true
			}
		}
	}
	if changed {
		// The rules see the words as the variables the script set make them.
		parts := make([]string, len(c.Args))
		for i, a := range c.Args {
			if q, err := driver.QuoteArgv([]string{texts[i]}); lits[i] && err == nil {
				parts[i] = q
			} else {
				var b strings.Builder
				_ = syntax.NewPrinter().Print(&b, a)
				parts[i] = b.String()
			}
		}
		w.sc.resolved = append(w.sc.resolved, strings.Join(parts, " "))
	}
	wrapped, affected := false, false
	remote := lits[0] && remoteRunners[programName(texts[0])]
	for i, a := range c.Args {
		program := i == 0 || wrapped
		name := ""
		switch {
		case lits[i]:
			name = programName(texts[i])
		case program:
			base, known := w.sc.base(a)
			switch {
			case known:
				name = programName(base)
			case w.sc.scriptSet(a) || w.sc.writtenUnknown:
				w.fail("a program named by a variable the script set when it ran, or by one that may name a file it wrote")
				return
			case in:
				w.fail("a program named by an expansion could read the statements that follow as something other than shell")
				return
			default:
				w.fail("a program named by an expansion whose value is not known here, which no rule can read")
				return
			}
		}
		if name == "" {
			continue
		}
		if program {
			// PATH may name the folder it went to, so the base name is enough.
			if base, ok := w.sc.base(a); ok && w.sc.written[base] {
				w.fail("it runs " + base + ", a file the script wrote, which no rule has read")
				return
			}
		}
		switch {
		case program && name == "eval":
			for j := i + 1; j < len(c.Args); j++ {
				if !lits[j] {
					w.fail("eval of text built when it runs")
					return
				}
			}
			w.inline(strings.Join(texts[i+1:], " "), in, cond)
			return
		case program && name == "trap":
			j := i + 1
			for j < len(c.Args) && lits[j] && (texts[j] == "--" || texts[j] == "-p" || texts[j] == "-l") {
				j++
			}
			if j+1 < len(c.Args) {
				if !lits[j] {
					w.fail("trap of text built when it runs")
					return
				}
				// Run later, in this shell, with its input.
				w.inline(texts[j], true, true)
			}
			return
		case program && (name == "source" || name == "."):
			if i+1 < len(c.Args) {
				switch next := path.Base(texts[i+1]); {
				case !lits[i+1]:
					w.fail(name + " of a file named when it runs")
				case stdinPath(texts[i+1]):
					w.fail(name + " of the shell's own input")
				case w.sc.written[next]:
					w.fail(name + " of " + next + ", a file the script wrote, which no rule has read")
				}
			}
			return
		case program && name == "alias" && i+1 < len(c.Args):
			w.fail("an alias, which changes what later statements run")
			return
		case program && name == "command" && i+1 < len(c.Args) && (texts[i+1] == "-v" || texts[i+1] == "-V"):
			return // only says what a name is
		case stdinInterpreters[name] && (program || remote && !stdinProgramOnly[name]):
			if w.interpreter(name, c, texts, lits, i, in, input); w.why != "" {
				return
			}
			continue
		case program && stdinWrappers[name]:
			wrapped = true
			continue
		case program && strings.Contains(texts[i], "="):
			if n, _, _ := strings.Cut(texts[i], "="); shellStartFiles[n] {
				w.fail(n + " is set, which a shell reads as commands or files to run on its own")
				return
			}
		}
		if program && !affected && !strings.HasPrefix(name, "-") && !strings.Contains(name, "=") {
			affected = true
			w.effects(name, c.Args[i+1:], texts[i+1:], lits[i+1:])
		}
	}
}

// inline walks text the shell runs as statements of its own, as eval's.
func (w *scriptWalk) inline(text string, in, cond bool) {
	f, err := parseScript([]byte(text))
	if err != nil {
		w.fail("text run as statements cannot be parsed")
		return
	}
	for _, st := range f.Stmts {
		w.stmt(st, in, cond)
	}
}

// remoteRunners run a program named among their arguments elsewhere, fed
// their own input.
var remoteRunners = map[string]bool{"kubectl": true, "oc": true, "docker": true, "podman": true, "nerdctl": true,
	"crictl": true, "ctr": true, "lxc": true, "incus": true, "runc": true, "machinectl": true, "systemd-nspawn": true}

// interpreter judges a shell or another program that runs what it reads.
func (w *scriptWalk) interpreter(name string, c *syntax.CallExpr, texts []string, lits []bool, i int, in bool,
	input *syntax.Redirect) {
	var code []int
	for j := i + 1; j+1 < len(c.Args); j++ {
		if k := codeWord(name, texts, lits, j); k > 0 {
			if !lits[k] {
				w.fail(name + " given code to run that is built when it runs")
				return
			}
			code = append(code, k)
		}
	}
	for _, a := range c.Args[i+1:] {
		if t := w.sc.text(a); stdinPath(t) || strings.Contains(t, "/fd/") {
			w.fail(name + " given the shell's own input to read")
			return
		}
		b, ok := w.sc.base(a)
		if ok && w.sc.written[b] {
			w.fail(name + " runs " + b + ", a file the script wrote, which no rule has read")
			return
		}
		if !ok && (w.sc.writtenUnknown || w.sc.scriptSet(a)) {
			w.fail(name + " given a file named when it runs, which may be one the script wrote")
			return
		}
	}
	if in {
		w.fail(name + " would read the statements that follow from the shell's own input, which are judged as shell, not as its input")
		return
	}
	if stdinShells[name] || name == "busybox" {
		for _, k := range code {
			if w.shellCode(name, texts[k], w.prefixed(c, texts[:i], lits[:i])); w.why != "" {
				return
			}
		}
	}
	if input == nil {
		return
	}
	switch input.Op {
	case syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
		body := input.Hdoc
		if input.Op == syntax.WordHdoc {
			body = input.Word
		}
		if !stdinShells[name] && name != "busybox" {
			w.fail("a here-document fed to " + name + ", which reads it as something other than shell")
			return
		}
		text, ok := literalWord(body)
		if !ok || body == nil {
			w.fail("a here-document fed to " + name + " is built when it runs")
			return
		}
		f, err := parseScript([]byte(text))
		if err != nil {
			w.fail("a here-document fed to " + name + " cannot be parsed")
			return
		}
		// The rules saw its text with the statement; here its own reads.
		inner := w.sc.clone()
		if why := inner.check(f); why != "" {
			w.fail(why + " (in a here-document fed to " + name + ")")
			return
		}
		w.sc.resolved = append(w.sc.resolved, inner.resolved...)
	default:
		if v, ok := w.sc.literal(input.Word); !ok || w.sc.written[path.Base(v)] {
			w.fail(name + " reads its commands from a file the script wrote or names when it runs")
		}
	}
}

// shellCode walks the text a shell runs from -c as a script of its own, so
// a program it names by a variable or a substitution is judged as one fed
// on stdin is.
func (w *scriptWalk) shellCode(name, text string, env map[string]*string) {
	f, err := parseScript([]byte(text))
	if err != nil {
		w.fail("the text " + name + " -c runs cannot be parsed")
		return
	}
	inner := &scriptWalk{sc: w.sc.clone()}
	maps.Copy(inner.sc.vars, env)
	inner.stmts(f.Stmts, false, false)
	if inner.why != "" {
		w.fail(inner.why + " (in the text " + name + " -c runs)")
		return
	}
	w.sc.resolved = append(w.sc.resolved, inner.sc.resolved...)
}

// prefixed is what a command's own assignments set for the program it runs:
// NAME=value before it, and env's NAME=value and -u NAME operands; nil for
// a value that is not known text.
func (w *scriptWalk) prefixed(c *syntax.CallExpr, texts []string, lits []bool) map[string]*string {
	env := map[string]*string{}
	for _, a := range c.Assigns {
		if a.Name == nil {
			continue
		}
		var v *string
		if t, ok := w.sc.literal(a.Value); ok || a.Value == nil {
			v = &t
		}
		env[a.Name.Value] = v
	}
	for j := 0; j < len(texts); j++ {
		k, v, ok := strings.Cut(texts[j], "=")
		switch {
		case texts[j] == "-u" || texts[j] == "--unset":
			if j+1 < len(texts) {
				env[texts[j+1]] = nil
				j++
			}
		case ok && validName(k):
			if lits[j] {
				env[k] = &v
			} else {
				env[k] = nil
			}
		}
	}
	return env
}

// seed sets the variables an exec gives the shell as known text, as the
// caller chose them, so a statement naming its program by one is read.
func (sc *stdinScript) seed(env []string) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && validName(k) {
			sc.vars[k] = &v
		}
	}
}

// codeWord is the index of the code an inline-code option at j gives the
// interpreter, -1 when texts[j] is none: for a shell, the first operand
// after its options and --, as the shell reads it.
func codeWord(name string, texts []string, lits []bool, j int) int {
	if !lits[j] || !inlineCode(name, texts[j]) {
		return -1
	}
	if stdinShells[name] || name == "busybox" {
		if k := tools.ShellCommandString(texts[j:], lits[j:]); k > 0 {
			return j + k
		}
		return -1
	}
	return j + 1
}

// inlineCode reports an option whose value is code the interpreter runs.
func inlineCode(name, opt string) bool {
	if stdinShells[name] || name == "busybox" {
		return len(opt) > 1 && opt[0] == '-' && opt[1] != '-' && strings.Contains(opt, "c")
	}
	switch opt {
	case "-c", "-e", "-E", "-r", "-p", "--eval", "--command", "--execute":
		return true
	}
	return false
}

// effects notes what a command sets for later statements: variables it
// reads into, and files it writes.
func (w *scriptWalk) effects(name string, args []*syntax.Word, texts []string, lits []bool) {
	operands := func() []int {
		var idx []int
		for j, t := range texts {
			if !lits[j] || !strings.HasPrefix(t, "-") {
				idx = append(idx, j)
			}
		}
		return idx
	}
	switch name {
	case "read", "mapfile", "readarray", "getopts", "printf", "unset", "export", "readonly", "declare", "typeset", "local", "let":
		for j, t := range texts {
			if !lits[j] {
				if name != "printf" && name != "let" {
					w.fail(name + " of a variable named when it runs")
				}
				continue
			}
			n, _, _ := strings.Cut(t, "=")
			if validName(n) {
				if name == "printf" && (j == 0 || texts[j-1] != "-v") {
					continue
				}
				w.set(n, nil)
			}
		}
		if name == "read" {
			w.set("REPLY", nil)
		}
	case "tee":
		for _, j := range operands() {
			w.wrote(args[j])
		}
	case "cp", "mv", "ln", "install", "rsync":
		for _, j := range operands() {
			w.wrote(args[j])
		}
	case "dd":
		for j, t := range texts {
			if !lits[j] || strings.HasPrefix(t, "of=") {
				if b, ok := w.sc.base(args[j]); ok {
					w.sc.written[strings.TrimPrefix(b, "of=")] = true
				}
			}
		}
	case "curl", "wget":
		for j, t := range texts {
			if (t == "-o" || t == "-O" || t == "--output" || t == "--output-document") && j+1 < len(args) {
				w.wrote(args[j+1])
			}
		}
	}
}

func validName(n string) bool {
	if n == "" || n[0] >= '0' && n[0] <= '9' {
		return false
	}
	for _, r := range n {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// Unknown text in a word is marked: quietUnknown where it stays one word,
// splitUnknown where the shell may split it into several or glob it.
const (
	quietUnknown = "\x00"
	splitUnknown = "\x01"
)

// literal is a word's text with the variables the script set to known text
// put in, false when any part is not known text.
func (sc *stdinScript) literal(w *syntax.Word) (string, bool) {
	if w == nil {
		return "", false
	}
	t := sc.text(w)
	return t, !strings.ContainsAny(t, quietUnknown+splitUnknown)
}

// base is the last path element of a word when that much is known text,
// as "$HOME/bin/tool" names tool whatever HOME holds.
func (sc *stdinScript) base(w *syntax.Word) (string, bool) {
	t := sc.text(w)
	b := t[strings.LastIndexByte(t, '/')+1:]
	return b, !strings.Contains(t, splitUnknown) && !strings.Contains(b, quietUnknown)
}

// scriptSet reports a word using a variable the script set, or a substitution.
func (sc *stdinScript) scriptSet(w *syntax.Word) bool {
	found := false
	syntax.Walk(w, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.ParamExp:
			// Positional parameters are set by set -- and by a function's caller.
			if _, set := sc.vars[paramName(n)]; set || !validName(paramName(n)) {
				found = true
			}
		case *syntax.CmdSubst, *syntax.ProcSubst:
			found = true
		}
		return !found
	})
	return found
}

// text is a word's text with the variables the script set put in, and
// unknown parts marked.
func (sc *stdinScript) text(w *syntax.Word) string {
	var b strings.Builder
	sc.parts(w.Parts, false, &b)
	return b.String()
}

func (sc *stdinScript) parts(ps []syntax.WordPart, quoted bool, b *strings.Builder) {
	miss := func() {
		if quoted {
			b.WriteString(quietUnknown)
		} else {
			b.WriteString(splitUnknown)
		}
	}
	for _, p := range ps {
		switch p := p.(type) {
		case *syntax.Lit:
			switch {
			case quoted:
				b.WriteString(p.Value)
			case globbing(p.Value):
				miss()
			default:
				b.WriteString(unescape(p.Value))
			}
		case *syntax.SglQuoted:
			if p.Dollar {
				miss()
				continue
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			sc.parts(p.Parts, true, b)
		case *syntax.ParamExp:
			v, set := sc.vars[paramName(p)]
			switch {
			case !simpleParam(p) || !set || v == nil:
				miss()
			case !quoted && (sc.ifs || strings.ContainsAny(*v, " \t\n*?["+quietUnknown+splitUnknown)):
				miss()
			default:
				b.WriteString(*v)
			}
		default:
			miss()
		}
	}
}

// globbing reports unquoted text the shell may expand into other words: a
// glob, or a brace list.
func globbing(v string) bool {
	if strings.ContainsAny(v, "*?") {
		return true
	}
	if i := strings.IndexByte(v, '['); i >= 0 && strings.IndexByte(v[i+1:], ']') >= 0 {
		return true
	}
	if i := strings.IndexByte(v, '{'); i >= 0 {
		rest := v[i+1:]
		if j := strings.IndexByte(rest, '}'); j >= 0 && (strings.Contains(rest[:j], ",") || strings.Contains(rest[:j], "..")) {
			return true
		}
	}
	return false
}

func paramName(p *syntax.ParamExp) string {
	if p.Param == nil {
		return ""
	}
	return p.Param.Value
}

// simpleParam is $NAME or ${NAME} with no operator.
func simpleParam(p *syntax.ParamExp) bool {
	return p.Param != nil && p.Flags == nil && p.NestedParam == nil && !p.Excl && !p.Length && !p.Width && !p.IsSet &&
		p.Index == nil && p.Slice == nil && len(p.Modifiers) == 0 && p.Repl == nil && p.Exp == nil && p.Names == 0 &&
		validName(p.Param.Value)
}

// unescape undoes backslashes in unquoted text.
func unescape(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			i++
		}
		b.WriteByte(v[i])
	}
	return b.String()
}
