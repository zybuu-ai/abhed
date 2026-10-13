package tools

import (
	"maps"
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// The variables a line sets to values the check can read, so a script it
// sources or runs by a path built from one ($X/x.sh) is read, not passed.

// stmtVars records what st sets before it runs. Only plain forms are
// followed: NAME=value statements, export NAME=value, and NAME=value before
// a command; after any other statement no value is known any more.
func (f *shellFacts) stmtVars(st *syntax.Stmt) {
	if f.sawOther {
		f.varsDead = true
	}
	plain := len(st.Redirs) == 0 && !st.Background && !st.Negated
	switch c := st.Cmd.(type) {
	case *syntax.CallExpr:
		f.setsEnv = f.setsEnv || len(c.Assigns) > 0
		for _, a := range c.Assigns {
			plain = plain && plainAssign(a)
			if a.Name != nil && (a.Name.Value == "BASH_ENV" || a.Name.Value == "ENV") {
				f.varsDead = true // a start-up file may set anything
			}
		}
		if len(c.Args) > 0 {
			// The command's own assignments are its environment (commandEnv),
			// unless one is not plain.
			if !plain {
				f.varsDead = true
			}
			f.sawOther = true
			return
		}
		if plain {
			for _, a := range c.Assigns {
				f.setVar(a, false)
			}
		}
	case *syntax.DeclClause:
		f.setsEnv = true
		plain = plain && c.Variant.Value == "export"
		for _, a := range c.Args {
			plain = plain && plainAssign(a)
		}
		if plain {
			for _, a := range c.Args {
				f.setVar(a, true)
			}
		}
	default:
		plain = false
	}
	if !plain {
		f.varsDead, f.sawOther = true, true
	}
}

// plainAssign reports NAME=value with an unquoted name and a value the
// check can read, the one assignment form followed.
func plainAssign(a *syntax.Assign) bool {
	return a.Name != nil && !a.Naked && !a.Append && a.Index == nil && a.Array == nil
}

// setVar records one assignment: its value when the check can read it, and
// otherwise that the name's value is not known.
func (f *shellFacts) setVar(a *syntax.Assign, export bool) {
	if a.Name == nil || a.Naked {
		return
	}
	name := a.Name.Value
	v, ok := "", a.Value == nil && a.Index == nil && a.Array == nil
	if a.Value != nil && a.Index == nil && a.Array == nil && !a.Append {
		v, ok = f.resolveWord(a.Value)
	}
	if ok {
		f.vars[name] = v
		delete(f.forgotten, name)
	} else {
		delete(f.vars, name)
		f.forgotten[name] = true
	}
	if export {
		f.exported[name] = true
	}
}

// forgetVar marks a variable set by a command (read, printf -v) as not
// known, and with it every other: what follows is not followed.
func (f *shellFacts) forgetVar(name string) {
	if f.vars != nil {
		delete(f.vars, name)
		f.forgotten[name] = true
	}
	f.varsDead = true
}

// commandEnv is the environment a command started by c gets: what the line
// exported, and c's own assignments.
func (f *shellFacts) commandEnv(c *syntax.CallExpr) map[string]string {
	env := map[string]string{}
	if f.varsDead {
		return env
	}
	for k := range f.exported {
		if v, ok := f.vars[k]; ok {
			env[k] = v
		}
	}
	for k, v := range f.inherited {
		if _, set := f.vars[k]; !set {
			if _, gone := f.forgotten[k]; !gone {
				env[k] = v
			}
		}
	}
	if c != nil {
		for _, a := range c.Assigns {
			if a.Name == nil || a.Naked {
				continue
			}
			if v, ok := f.resolveWord(a.Value); ok && a.Value != nil && a.Index == nil && a.Array == nil {
				env[a.Name.Value] = v
			} else {
				delete(env, a.Name.Value)
			}
		}
	}
	return env
}

// resolveWord is a word's value when every part is literal or a plain
// variable the line or the environment it is read in gives a known value.
func (f *shellFacts) resolveWord(w *syntax.Word) (string, bool) {
	if w == nil {
		return "", true
	}
	var b strings.Builder
	var parts func([]syntax.WordPart, bool) bool
	parts = func(ps []syntax.WordPart, quoted bool) bool {
		for i, p := range ps {
			switch p := p.(type) {
			case *syntax.Lit:
				v := p.Value
				if !quoted && i == 0 && b.Len() == 0 && (v == "~" || strings.HasPrefix(v, "~/")) {
					home, ok := f.lookup("HOME")
					if !ok {
						return false
					}
					v = home + v[1:]
				}
				if quoted {
					b.WriteString(unescape(v, "$`\"\\\n"))
				} else {
					b.WriteString(unescape(v, ""))
				}
			case *syntax.SglQuoted:
				if p.Dollar {
					return false
				}
				b.WriteString(p.Value)
			case *syntax.DblQuoted:
				if !parts(p.Parts, true) {
					return false
				}
			case *syntax.ParamExp:
				if !simpleParam(p) {
					return false
				}
				v, ok := f.lookup(p.Param.Value)
				if !ok {
					return false
				}
				b.WriteString(v)
			default:
				return false
			}
		}
		return true
	}
	if !parts(w.Parts, false) {
		return "", false
	}
	return b.String(), true
}

// lookup is a variable's known value: set on the line, or given by the
// environment the text is read in.
func (f *shellFacts) lookup(name string) (string, bool) {
	if f.varsDead {
		return "", false
	}
	if v, ok := f.vars[name]; ok {
		return v, true
	}
	if _, gone := f.forgotten[name]; gone {
		return "", false
	}
	v, ok := f.inherited[name]
	return v, ok
}

// withEnv seeds f with the environment its text runs in.
func (f *shellFacts) withEnv(env map[string]string) {
	f.inherited = maps.Clone(env)
	f.vars, f.exported, f.forgotten = map[string]string{}, map[string]bool{}, map[string]bool{}
}

// SetsEnv reports a line that changes the environment a shell it starts
// gets: an assignment, export or env, or a cd before it.
func (c Canonical) SetsEnv() bool { return c.shell.setsEnv || c.shell.moves }

// ScriptHidden says why a script the command sources or runs is named by an
// expansion the check cannot resolve; "" when each is known.
func (c Canonical) ScriptHidden() string { return c.shell.scriptHidden }

// Env is the environment a command the line starts last gets, as far as
// the line shows it.
func (c Canonical) Env() map[string]string { return c.shell.startEnv() }

// CanonicalCommandEnv is CanonicalCommand for text run in the environment
// env, whose variables resolve the paths of the scripts it runs.
func CanonicalCommandEnv(command string, env map[string]string) Canonical {
	c := CanonicalCommand(command)
	if len(env) > 0 {
		c.shell = parseShellEnv(command, env)
	}
	return c
}

// plainShellStart reports a file that is a single simple command whose
// program is a shell named literally, with no assignment or redirection.
func plainShellStart(file *syntax.File) bool {
	if len(file.Stmts) != 1 {
		return false
	}
	st := file.Stmts[0]
	c, ok := st.Cmd.(*syntax.CallExpr)
	if !ok || len(c.Assigns) > 0 || len(st.Redirs) > 0 || st.Background || st.Negated || len(c.Args) == 0 {
		return false
	}
	name, lit := literal(c.Args[0])
	_, shell := homeStartFiles[path.Base(name)]
	return lit && shell
}

// BaselineApplies reports a line the person's start-up baseline may be
// applied to: one simple command starting a shell, which changes nothing
// the shell's start-up files could read, here or in the text it runs.
func (c Canonical) BaselineApplies() bool {
	return c.shell.plainShell && !c.shell.innerShell && !c.SetsEnv() && !c.shell.unparsed
}

// AskUnderRules says why the line is put to a person under a deny or ask
// rule for bash, though it hides nothing; "" for most lines.
func (c Canonical) AskUnderRules() string { return c.shell.askUnderRules }
