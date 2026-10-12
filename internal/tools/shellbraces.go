package tools

import (
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// braceBudget bounds the words brace expansion may make across one command,
// nested text included; past it a word with braces hides what it makes.
const braceBudget = 16 << 10

// braces reads the command c's words make once their braces are expanded,
// as a command of its own.
func (f *shellFacts) braces(c *syntax.CallExpr, st *syntax.Stmt, depth int) {
	if f.inBraces || !hasBraceText(c.Args) {
		return
	}
	readings := [2][]*syntax.Word{}
	expanded, differ := false, false
	for _, w := range c.Args {
		split := &syntax.Word{Parts: append([]syntax.WordPart(nil), w.Parts...)}
		if !syntax.SplitBraces(split) {
			readings[0], readings[1] = append(readings[0], w), append(readings[1], w)
			if braceLike(w) {
				f.braceHidden = true
			}
			continue
		}
		expanded = true
		words, ok := f.expandBraces(split)
		// bash 3.2 keeps a sequence with a step as written and drops zero
		// padding; later versions do neither. Each reading is a command.
		old := words
		if o := oldBashBraces(split); ok && o != split {
			differ = true
			old, ok = f.expandBraces(o)
		}
		if !ok {
			f.braceHidden = true
			return
		}
		readings[0], readings[1] = append(readings[0], words...), append(readings[1], old...)
	}
	if !expanded {
		return
	}
	f.inBraces, f.braced = true, true
	defer func() { f.inBraces = false }()
	f.call(&syntax.CallExpr{Args: readings[0]}, st, depth)
	if differ {
		f.call(&syntax.CallExpr{Args: readings[1]}, st, depth)
	}
}

// expandBraces is the words word makes, within what is left of the budget.
func (f *shellFacts) expandBraces(word *syntax.Word) ([]*syntax.Word, bool) {
	var words []*syntax.Word
	for w, err := range expand.BracesSeq(nil, word) {
		if err != nil || f.braceWords >= braceBudget {
			return nil, false
		}
		f.braceWords++
		words = append(words, w)
	}
	return words, true
}

// hasBraceText reports a word with an unquoted { and a } after it: the only
// words brace expansion may change, checked before parsing them.
func hasBraceText(args []*syntax.Word) bool {
	for _, w := range args {
		open := false
		for _, p := range w.Parts {
			if l, ok := p.(*syntax.Lit); ok {
				i := strings.IndexByte(l.Value, '{')
				open = open || i >= 0
				if open && strings.IndexByte(l.Value[max(i, 0):], '}') >= 0 {
					return true
				}
			}
		}
	}
	return false
}

// braceLike reports a word the brace parser left alone that still holds an
// unquoted {…} with a comma or .. in it, which bash might read apart.
func braceLike(w *syntax.Word) bool {
	var b strings.Builder
	for _, p := range w.Parts {
		if l, ok := p.(*syntax.Lit); ok {
			b.WriteString(l.Value)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	for i := strings.IndexByte(s, '{'); i >= 0; {
		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			return false
		}
		if in := s[i : i+end]; (strings.Contains(in, ",") || strings.Contains(in, "..")) && (i == 0 || s[i-1] != '\\') {
			return true
		}
		next := strings.IndexByte(s[i+1:], '{')
		if next < 0 {
			return false
		}
		i += next + 1
	}
	return false
}

// oldBashBraces is word as bash 3.2 reads its sequences: one with a step is
// kept as written, and zero padding is dropped. It is word when neither occurs.
func oldBashBraces(word *syntax.Word) *syntax.Word {
	changed := false
	var parts func([]syntax.WordPart) []syntax.WordPart
	parts = func(in []syntax.WordPart) []syntax.WordPart {
		out := make([]syntax.WordPart, 0, len(in))
		for _, p := range in {
			br, ok := p.(*syntax.BraceExp)
			if !ok {
				out = append(out, p)
				continue
			}
			nb := &syntax.BraceExp{Sequence: br.Sequence}
			switch {
			case br.Sequence && len(br.Elems) > 2:
				changed = true
				lits := make([]string, len(br.Elems))
				for k, e := range br.Elems {
					lits[k] = e.Lit()
				}
				out = append(out, &syntax.Lit{Value: "{" + strings.Join(lits, "..") + "}"})
				continue
			case br.Sequence:
				for _, e := range br.Elems {
					v := e.Lit()
					if u := unpadded(v); u != v {
						changed, v = true, u
					}
					nb.Elems = append(nb.Elems, &syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{Value: v}}})
				}
			default:
				for _, e := range br.Elems {
					nb.Elems = append(nb.Elems, &syntax.Word{Parts: parts(e.Parts)})
				}
			}
			out = append(out, nb)
		}
		return out
	}
	old := &syntax.Word{Parts: parts(word.Parts)}
	if !changed {
		return word
	}
	return old
}

// unpadded is a sequence end without its leading zeros: -007 is -7.
func unpadded(v string) string {
	sign, n := "", v
	if strings.HasPrefix(n, "-") {
		sign, n = "-", n[1:]
	}
	if len(n) < 2 || n[0] != '0' || strings.Trim(n, "0123456789") != "" {
		return v
	}
	if n = strings.TrimLeft(n, "0"); n == "" {
		n = "0"
	}
	return sign + n
}
