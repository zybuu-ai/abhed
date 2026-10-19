package monitor

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// command is one simple command of a shell line, as the rules read it.
type command struct {
	// words are the command's words with quotes removed; a word whose value
	// only running the line would give (a variable, a substitution) is kept
	// as written, with its leading $ so it is never taken for a name.
	words []string
	// writes are the files its redirections write.
	writes []string
}

// script is what the rules read from a shell line.
type script struct {
	cmds []command
	// funcs are the functions the line defines; calling one is not running a program.
	funcs map[string]bool
	// setsPath is set when the line changes PATH, so which program runs is unknown.
	setsPath bool
}

// parseScript reads a shell line. ok is false for a line the parser refuses,
// which the rules then leave to the person and the sandbox.
func parseScript(src string) (script, bool) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil {
		return script{}, false
	}
	s := script{funcs: map[string]bool{}}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.FuncDecl:
			s.funcs[x.Name.Value] = true
		case *syntax.Assign:
			if x.Name != nil && x.Name.Value == "PATH" {
				s.setsPath = true
			}
		case *syntax.Stmt:
			call, ok := x.Cmd.(*syntax.CallExpr)
			if !ok {
				return true
			}
			var c command
			for _, w := range call.Args {
				c.words = append(c.words, wordText(src, w))
			}
			for _, r := range x.Redirs {
				switch r.Op {
				case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll, syntax.RdrClob:
					if r.Word != nil {
						c.writes = append(c.writes, wordText(src, r.Word))
					}
				}
			}
			if len(c.words) > 0 || len(c.writes) > 0 {
				s.cmds = append(s.cmds, c)
			}
		}
		return true
	})
	return s, true
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
var wrappers = map[string]bool{"sudo": true, "env": true, "nohup": true, "time": true, "nice": true, "timeout": true,
	"exec": true, "builtin": true, "stdbuf": true, "ionice": true, "chrt": true, "doas": true}

// wrapperValueFlags are a wrapper's flags that take the next word as their value.
var wrapperValueFlags = map[string]bool{"-u": true, "-g": true, "-n": true, "-s": true, "-k": true, "-C": true,
	"-o": true, "-e": true, "-i": true, "-c": true, "-p": true}

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
