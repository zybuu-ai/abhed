package tools

import (
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// PipelineWords splits a command meant for another machine's login shell
// into the words of each command in a plain pipeline (a | b | c), quotes
// undone. It reads only text every common login shell (sh, bash, dash, zsh,
// ksh, fish, csh, busybox ash) reads the same way: plain words, single
// quotes, and double quotes with nothing the shell would expand. Anything
// else, a redirection, an expansion, a list or a background job, is refused
// with why; why is "" for a pipeline it read.
func PipelineWords(command string) (stages [][]string, why string) {
	if why := portableText(command); why != "" {
		return nil, why
	}
	if len(command) > maxShellBytes {
		return nil, "the command is too long to read"
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, "the command cannot be parsed as a POSIX shell reads it"
	}
	if len(file.Stmts) != 1 {
		return nil, "the command is not one pipeline"
	}
	var walk func(st *syntax.Stmt) string
	walk = func(st *syntax.Stmt) string {
		if st.Negated || st.Background || st.Coprocess || len(st.Redirs) > 0 {
			return "the command has a redirection, a ! or a background job"
		}
		switch c := st.Cmd.(type) {
		case *syntax.BinaryCmd:
			if c.Op != syntax.Pipe {
				return "the command joins commands with " + c.Op.String() + ", not only |"
			}
			if why := walk(c.X); why != "" {
				return why
			}
			return walk(c.Y)
		case *syntax.CallExpr:
			if len(c.Assigns) > 0 {
				return "the command sets a variable"
			}
			words := make([]string, 0, len(c.Args))
			for i, w := range c.Args {
				v, ok := literal(w)
				if !ok {
					return "a word holds an expansion"
				}
				// A glob the remote shell could expand into an option: a
				// file named -delete or --output=x would become one.
				if globWord(w) && (i == 0 || strings.HasPrefix(v, "-") || v != "" && strings.ContainsAny(v[:1], "*?[")) {
					return fmt.Sprintf("%q is a pattern the remote shell could expand into an option; write it after a path, such as ./%s", v, v)
				}
				words = append(words, v)
			}
			if len(words) == 0 {
				return "a command in the pipeline is empty"
			}
			stages = append(stages, words)
			return ""
		default:
			return "the command is not a plain pipeline of simple commands"
		}
	}
	if why := walk(file.Stmts[0]); why != "" {
		return nil, why
	}
	return stages, ""
}

// portableText reports text a login shell other than bash could read
// differently, or that runs anything but a pipeline, or "".
func portableText(s string) string {
	if strings.TrimSpace(s) == "" {
		return "the command is empty"
	}
	word := true // at the start of a word
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			word = true
			continue
		case c == '\'':
			// fish reads \' and \\ inside single quotes; the others keep them.
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return "a single quote is not closed"
			}
			if q := s[i+1 : i+1+j]; strings.ContainsAny(q, "\\\n\r") {
				return "a single-quoted word holds a backslash or a line break, which login shells read differently"
			}
			i += j + 1
		case c == '"':
			j := strings.IndexByte(s[i+1:], '"')
			if j < 0 {
				return "a double quote is not closed"
			}
			for _, r := range s[i+1 : i+1+j] {
				if !portableQuoted(r) {
					return fmt.Sprintf("a double-quoted word holds %q, which some login shell would expand or read differently; use single quotes", r)
				}
			}
			i += j + 1
		case c == '|':
			if i+1 < len(s) && (s[i+1] == '|' || s[i+1] == '&') {
				return "the command uses || or |&, not only |"
			}
			word = true
			continue
		case c == '\\':
			// find's terminator, \; as the last word of its command.
			if word && i+1 < len(s) && s[i+1] == ';' && endsStage(s[i+2:]) {
				i++
				word = false
				continue
			}
			return "a backslash outside quotes (only \\; ending find -exec is read)"
		case c == '{':
			// find's {} as a word of its own; braces elsewhere expand in some shells.
			if word && i+1 < len(s) && s[i+1] == '}' && (i+2 == len(s) || strings.IndexByte(" \t|", s[i+2]) >= 0) {
				i++
				word = false
				continue
			}
			return "a brace outside quotes (only {} for find -exec is read)"
		case c == '=' && word:
			return "a word starts with =, which zsh expands to a program's path"
		case plainChar(c):
		case c == '<' || c == '>':
			return "a redirection (< or >), which may write a file"
		case c == '$' || c == '`':
			return "an expansion or command substitution ($ or `), whose text is not known until it runs"
		case c == ';' || c == '&' || c == '\n' || c == '\r':
			return "a list or background job (; & or a line break), not only a pipeline"
		case c == '(' || c == ')':
			return "a subshell or substitution (( or ))"
		default:
			return fmt.Sprintf("%q outside single quotes, which a shell reads as more than text", string(c))
		}
		word = false
	}
	return ""
}

// endsStage reports rest, the text after a word, as the end of the
// command or of its stage in the pipeline.
func endsStage(rest string) bool {
	rest = strings.TrimLeft(rest, " \t")
	return rest == "" || rest[0] == '|'
}

// plainChar is a byte every login shell takes as itself outside quotes;
// the glob characters are judged once the words are known.
func plainChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("-_./:=,+@%~*?[]", c) >= 0
}

// portableQuoted is a rune double quotes keep as itself in every login shell:
// no $, `, \ or ! (expanded or escaped by one or another), and none of
// the characters rc, which has no double quotes, would read as syntax.
func portableQuoted(r rune) bool {
	if r > '~' {
		return false
	}
	return r == ' ' || r == '\t' || r < 128 && plainChar(byte(r))
}
