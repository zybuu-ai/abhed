package tools

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Canonical is a shell command with the tricks that hide its words undone,
// for the policy checks only: the command runs as written.
type Canonical struct {
	// Text is the command with line continuations joined, IFS expansions and
	// brace lists turned into spaces, and $'...' decoded.
	Text string
	// Joined reports that a backslash-newline continuation was removed.
	Joined bool
	// Reworded reports any change beyond joining; no allow rule may match then.
	Reworded bool
	// Hidden reports words split or glued by an expansion, a brace list or
	// IFS, which a rule may not see; a check with rules must fail closed.
	Hidden bool
}

// CanonicalCommand reads command as the shell would split it into words, as
// far as text can: POSIX removes a backslash-newline everywhere but in single quotes.
func CanonicalCommand(command string) Canonical {
	var c Canonical
	var b strings.Builder
	b.Grow(len(command))
	wordStart := func() bool {
		// strings.Builder.String does not copy.
		s := b.String()
		return s == "" || strings.ContainsRune(" \t\n;&|()`<>", rune(s[len(s)-1]))
	}
	double := false
	for i := 0; i < len(command); i++ {
		ch := command[i]
		switch {
		case ch == '\\' && i+1 < len(command) && command[i+1] == '\n':
			c.Joined = true
			i++
		case ch == '\\' && i+1 < len(command):
			b.WriteString(command[i : i+2])
			i++
		case ch == '"':
			double = !double
			b.WriteByte(ch)
		case ch == '\'' && !double:
			j := strings.IndexByte(command[i+1:], '\'')
			if j < 0 {
				b.WriteString(command[i:])
				i = len(command)
				continue
			}
			b.WriteString(command[i : i+j+2])
			i += j + 1
		case ch == '$' && !double && i+1 < len(command) && command[i+1] == '\'':
			text, end := ansiC(command, i+2)
			if strings.ContainsAny(text, " \t\n\r\f\v") {
				c.Hidden = true
			}
			text = strings.Map(blankToSpace, text)
			c.Reworded = true
			b.WriteString(text)
			i = end
		case ch == '$' && ifsAt(command, i+1) > 0:
			b.WriteByte(' ')
			c.Reworded, c.Hidden = true, true
			i += ifsAt(command, i+1)
		case ch == '{' && !double && (i == 0 || command[i-1] != '$'):
			if end := braceList(command, i); end > 0 {
				b.WriteString(strings.NewReplacer(",", " ").Replace(command[i+1 : end]))
				b.WriteByte(' ')
				c.Reworded, c.Hidden = true, true
				i = end
				continue
			}
			b.WriteByte(ch)
		case ch == '\t' || ch == '\r' || ch == '\f' || ch == '\v':
			b.WriteByte(' ')
			c.Reworded = true
		default:
			if !double && wordStart() && strings.HasPrefix(command[i:], "IFS=") && ifsValue(command[i+4:]) {
				c.Reworded, c.Hidden = true, true
			}
			b.WriteByte(ch)
		}
	}
	c.Text = b.String()
	return c
}

func blankToSpace(r rune) rune {
	if strings.ContainsRune("\t\n\r\f\v", r) {
		return ' '
	}
	return r
}

// ifsAt returns the length of an IFS expansion starting at command[i], just
// after its $: IFS, or {IFS...} with any operator; 0 when there is none.
func ifsAt(command string, i int) int {
	rest := command[i:]
	if strings.HasPrefix(rest, "IFS") && (len(rest) == 3 || !isNameByte(rest[3])) {
		return 3
	}
	if strings.HasPrefix(rest, "{IFS") && len(rest) > 4 && !isNameByte(rest[4]) {
		if end := strings.IndexByte(rest, '}'); end > 0 {
			return end + 1
		}
		return len(rest)
	}
	return 0
}

func isNameByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// ifsValue reports whether an IFS= assignment sets a non-empty value; the
// common IFS= read leaves no separator to split on.
func ifsValue(rest string) bool {
	return rest != "" && !strings.ContainsRune(" \t\n;&|", rune(rest[0]))
}

// braceList returns the index of the } closing a brace expansion with a comma
// that opens at command[i], or 0: {rm,-rf,x} expands to three words.
func braceList(command string, i int) int {
	depth, comma := 0, false
	for j := i; j < len(command); j++ {
		switch command[j] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				if comma {
					return j
				}
				return 0
			}
		case ',':
			comma = comma || depth == 1
		case ' ', '\t', '\n', ';', '&', '|', '\'', '"':
			return 0
		}
	}
	return 0
}

// ansiC decodes a $'...' string from command[i], just past its opening quote,
// and returns the text and the index of its closing quote.
func ansiC(command string, i int) (string, int) {
	var b strings.Builder
	for ; i < len(command); i++ {
		ch := command[i]
		if ch == '\'' {
			return b.String(), i
		}
		if ch != '\\' || i+1 >= len(command) {
			b.WriteByte(ch)
			continue
		}
		i++
		switch e := command[i]; e {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		case 'a', 'b', 'e', 'E':
			b.WriteByte(' ')
		case 'x', 'u', 'U':
			max := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
			j := i + 1
			for j < len(command) && j-i-1 < max && strings.IndexByte("0123456789abcdefABCDEF", command[j]) >= 0 {
				j++
			}
			if n, err := strconv.ParseUint(command[i+1:j], 16, 32); err == nil {
				b.WriteRune(rune(n))
				i = j - 1
			} else {
				b.WriteByte(e)
			}
		case '0', '1', '2', '3', '4', '5', '6', '7':
			j := i
			for j < len(command) && j-i < 3 && command[j] >= '0' && command[j] <= '7' {
				j++
			}
			n, _ := strconv.ParseUint(command[i:j], 8, 32)
			b.WriteRune(rune(n))
			i = j - 1
		default:
			b.WriteByte(e)
		}
	}
	return b.String(), len(command)
}

// ifsSet finds an IFS assignment with a value followed later by an expansion.
var ifsSet = regexp.MustCompile(`(?s)(?:^|[\s;&|(])IFS=[^\s;&|].*\$`)

// hiddenWords reports a command whose program, or whose split into words, is
// only known when it runs: an expansion as the program, or IFS set and expanded.
// The IFS assignment is read as written, the program in the canonical text.
func hiddenWords(command, canon string) (string, bool) {
	if ifsSet.MatchString(command) || ifsSet.MatchString(canon) {
		return "words split by a changed IFS", true
	}
	for _, part := range strings.Split(shellBreaks.Replace(collapseSubstitutions(canon)), "\n") {
		pos := position{on: true}
		for _, w := range strings.Fields(part) {
			if !pos.next(w) || w == gitSubstitution {
				continue
			}
			if namesExpansion(path.Base(shellQuotes.Replace(w))) {
				return "program named by an expansion", true
			}
		}
	}
	return "", false
}

// namesExpansion reports a $ or backtick whose value may be a command and its
// flags; $$, $?, $#, $! and $- only ever hold a number or option letters.
func namesExpansion(w string) bool {
	if strings.ContainsRune(w, '`') {
		return true
	}
	for i := 0; i < len(w); i++ {
		if w[i] == '$' && (i+1 == len(w) || !strings.ContainsRune("$?#!-", rune(w[i+1]))) {
			return true
		}
		if w[i] == '$' {
			i++
		}
	}
	return false
}
