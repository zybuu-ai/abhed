// Package sedscript reads a sed script far enough to tell whether it only
// edits text or can also run a command or write a file.
package sedscript

import (
	"errors"
	"strings"
)

// Script is what a parsed sed script does beyond editing its input.
type Script struct {
	// Runs holds the command text of each `e CMD`; RunsInput is set for a bare
	// `e` or an `s///e`, which run the edited input itself as a command.
	Runs      []string
	RunsInput bool
	// Writes is set for a w/W command or an s///w flag; Files are the files named.
	Writes bool
	Files  []string
}

// Inert reports a script that only edits text: it runs nothing and writes no file.
func (s Script) Inert() bool { return len(s.Runs) == 0 && !s.RunsInput && !s.Writes }

var errSyntax = errors.New("sed script not understood")

// Parse reads a GNU sed script (several -e values join with newlines). An
// error means the script was not understood and must not be taken as text.
func Parse(src string) (Script, error) {
	var s Script
	p := parser{src: src}
	for {
		p.skip(" \t\n;")
		if p.end() {
			return s, nil
		}
		if p.peek() == '}' {
			p.i++
			continue
		}
		if p.peek() == '#' {
			p.toEOL()
			continue
		}
		if err := p.address(); err != nil {
			return s, err
		}
		p.skip(" \t")
		for !p.end() && p.peek() == '!' {
			p.i++
			p.skip(" \t")
		}
		if p.end() {
			return s, errSyntax
		}
		c := p.next()
		switch c {
		case '{':
		case '=', 'd', 'D', 'g', 'G', 'h', 'H', 'n', 'N', 'p', 'P', 'x', 'z', 'F':
		case 'l', 'L', 'q', 'Q':
			p.skip(" \t")
			for !p.end() && p.peek() >= '0' && p.peek() <= '9' {
				p.i++
			}
		case ':', 'b', 't', 'T', 'v':
			p.label()
		case 'a', 'i', 'c', 'r', 'R':
			p.text() // appended/inserted text or a file it reads
		case 'e':
			if t := strings.TrimSpace(p.toEOL()); t != "" {
				s.Runs = append(s.Runs, t)
			} else {
				s.RunsInput = true
			}
		case 'w', 'W':
			s.Writes = true
			s.Files = append(s.Files, strings.TrimSpace(p.toEOL()))
		case 's':
			if p.end() || strings.IndexByte("\n\\", p.peek()) >= 0 {
				return s, errSyntax
			}
			d := p.next()
			if !p.until(d) {
				return s, errSyntax
			}
			if !p.until(d) {
				return s, errSyntax
			}
			for !p.end() {
				f := p.peek()
				if f == 'w' {
					p.i++
					s.Writes = true
					s.Files = append(s.Files, strings.TrimSpace(p.toEOL()))
					break
				}
				if f == 'e' {
					s.RunsInput = true
				} else if strings.IndexByte("gpiImM0123456789", f) < 0 {
					break
				}
				p.i++
			}
		case 'y':
			if p.end() || strings.IndexByte("\n\\", p.peek()) >= 0 {
				return s, errSyntax
			}
			d := p.next()
			if !p.until(d) {
				return s, errSyntax
			}
			if !p.until(d) {
				return s, errSyntax
			}
		default:
			return s, errSyntax
		}
		// A command ends at ;, a newline, } or a comment; anything else is not understood.
		p.skip(" \t")
		if !p.end() && strings.IndexByte(";\n}#", p.peek()) < 0 && c != '{' {
			return s, errSyntax
		}
	}
}

type parser struct {
	src string
	i   int
}

func (p *parser) end() bool  { return p.i >= len(p.src) }
func (p *parser) peek() byte { return p.src[p.i] }
func (p *parser) next() byte { c := p.src[p.i]; p.i++; return c }

func (p *parser) skip(set string) {
	for !p.end() && strings.IndexByte(set, p.peek()) >= 0 {
		p.i++
	}
}

func (p *parser) toEOL() string {
	j := strings.IndexByte(p.src[p.i:], '\n')
	if j < 0 {
		j = len(p.src) - p.i
	}
	t := p.src[p.i : p.i+j]
	p.i += j
	return t
}

// label reads a branch label or version, which ends at ; or a newline.
func (p *parser) label() {
	for !p.end() && p.peek() != ';' && p.peek() != '\n' {
		p.i++
	}
}

// text reads a/i/c text or an r/R file name: the rest of the line, and the
// lines after a trailing backslash.
func (p *parser) text() {
	for !p.end() {
		c := p.next()
		if c == '\\' && !p.end() {
			p.i++
			continue
		}
		if c == '\n' {
			return
		}
	}
}

// until reads past the next unescaped delimiter, as GNU sed does (no
// bracket-expression special case), and reports whether one was found.
func (p *parser) until(d byte) bool {
	for !p.end() {
		c := p.next()
		if c == '\\' && !p.end() {
			p.i++
			continue
		}
		if c == d {
			return true
		}
	}
	return false
}

// address reads an optional address or address range.
func (p *parser) address() error {
	if err := p.addr1(); err != nil {
		return err
	}
	p.skip(" \t")
	if !p.end() && p.peek() == ',' {
		p.i++
		p.skip(" \t")
		if !p.end() && (p.peek() == '+' || p.peek() == '~') {
			p.i++
		}
		if err := p.addr1(); err != nil {
			return err
		}
	}
	return nil
}

func (p *parser) addr1() error {
	if p.end() {
		return nil
	}
	switch c := p.peek(); {
	case c >= '0' && c <= '9':
		for !p.end() && (p.peek() >= '0' && p.peek() <= '9' || p.peek() == '~') {
			p.i++
		}
	case c == '$':
		p.i++
	case c == '/' || c == '\\':
		p.i++
		d := byte('/')
		if c == '\\' {
			if p.end() {
				return errSyntax
			}
			d = p.next()
		}
		if !p.until(d) {
			return errSyntax
		}
		for !p.end() && (p.peek() == 'I' || p.peek() == 'M') {
			p.i++
		}
	}
	return nil
}

// Scripts returns the script text sed takes from its arguments — each -e or
// --expression value joined by newlines, else the first operand — and false
// when the script is not all on the command line (-f, --file, or an option
// cluster the reading does not follow).
func Scripts(args []string) (string, bool) {
	var exprs []string
	first := ""
	haveFirst := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) && len(exprs) == 0 && !haveFirst {
				first, haveFirst = args[i+1], true
			}
			i = len(args)
		case a == "-e" || a == "--expression":
			if i+1 >= len(args) {
				return "", false
			}
			exprs = append(exprs, args[i+1])
			i++
		case strings.HasPrefix(a, "--expression="):
			exprs = append(exprs, strings.TrimPrefix(a, "--expression="))
		case a == "-f" || a == "--file" || strings.HasPrefix(a, "--file="):
			return "", false
		case a == "-l" || a == "--line-length":
			i++ // takes a number
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// A short cluster: e takes the script, f a file, i a suffix and
			// l a number, each attached or (e, f, l) as the next word.
		cluster:
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'e':
					if j == len(a)-1 {
						if i+1 >= len(args) {
							return "", false
						}
						exprs = append(exprs, args[i+1])
						i++
					} else {
						exprs = append(exprs, a[j+1:])
					}
					break cluster
				case 'f':
					return "", false
				case 'i':
					break cluster
				case 'l':
					if j == len(a)-1 {
						i++
					}
					break cluster
				}
			}
		default:
			if !haveFirst {
				first, haveFirst = a, true
			}
		}
	}
	if len(exprs) > 0 {
		return strings.Join(exprs, "\n"), true
	}
	return first, haveFirst
}
