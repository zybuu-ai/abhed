package tools

import (
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
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
	// IFS holds the separators beyond blanks that an IFS assignment sets; the
	// checks also read the command split on them.
	IFS string
	// IFSUnknown reports IFS set in a way the text does not show, such as
	// IFS=$v, read IFS or ${IFS:=x}.
	IFSUnknown bool
	// IFSSplit reports a changed IFS and an expansion it may split, so the
	// words that run are not in the text; deny rules cannot be checked then.
	IFSSplit bool
	// command is the text as written; shell is what a bash parser reads in it.
	command string
	shell   shellFacts
	// textHidden and textIFSUnknown are what the text checks alone found.
	textHidden, textIFSUnknown bool
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
	c.IFS, c.IFSUnknown = ifsChanges(command)
	c.textHidden, c.textIFSUnknown = c.Hidden, c.IFSUnknown
	// The parser reads the text as written; its findings are added to the
	// text checks', and a command it cannot read hides everything.
	c.command, c.shell = command, parseShell(command)
	c.IFS = blankless(c.IFS + c.shell.seps)
	c.IFSUnknown = c.IFSUnknown || c.shell.ifsUnknown || c.shell.unparsed
	c.Hidden = c.Hidden || c.shell.unparsed || c.shell.tooLong
	c.IFSSplit = (c.IFS != "" || c.IFSUnknown) && strings.ContainsAny(command, "$`")
	return c
}

// IFSSplitText is text with each separator in seps read as a blank, as the
// shell splits a value under that IFS.
func IFSSplitText(text, seps string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(seps, r) {
			return ' '
		}
		return r
	}, text)
}

var unsetBefore = regexp.MustCompile(`(?:^|[^\w])unset(?:[ \t]+-[vfn]+)*[ \t]+$`)

// ifsChanges reads every IFS in command, quoted or not since eval may run it:
// the separators assignments set beyond blanks, and whether one is unreadable.
// It also reads the text with quotes, backslashes and $'...' undone, as often
// as eval could undo them, and treats a name built from an expansion, given to
// eval, a shell's -c or a command that sets a variable, as setting IFS.
func ifsChanges(command string) (string, bool) {
	var seps strings.Builder
	unknown := builtNames(command)
	for text, n := command, 0; n < 8; n++ {
		s, u := ifsScan(text)
		seps.WriteString(s)
		unknown = unknown || u
		next := evalText(text)
		if next == text {
			break
		}
		text = next
	}
	return blankless(seps.String()), unknown
}

// blankless is seps without blanks or repeats: the separators that split
// words where the default IFS would not.
func blankless(seps string) string {
	var out strings.Builder
	for _, r := range seps {
		if !strings.ContainsRune(" \t\n"+out.String(), r) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// ifsScan reads each IFS in text as written: the separators its assignments
// set, and whether one sets it in a way the text does not show.
func ifsScan(text string) (string, bool) {
	command := text
	var seps strings.Builder
	unknown := false
	var sc ifsScanner
	for i := 0; i < len(command) && !unknown; {
		j := strings.Index(command[i:], "IFS")
		if j < 0 {
			break
		}
		at := i + j
		i = at + 3
		sc.advance(command, at)
		if i < len(command) && isNameByte(command[i]) {
			continue
		}
		if at > 0 && isNameByte(command[at-1]) {
			// printf -vIFS, read -aIFS: a name glued to its option.
			opt := command[sc.wordStart:at]
			glued := len(opt) > 1 && len(opt) < 64 && opt[0] == '-' && strings.IndexFunc(opt[1:], func(r rune) bool { return !unicode.IsLetter(r) }) < 0
			unknown = glued && sc.setter
			continue
		}
		before, rest := command[:at], command[i:]
		switch {
		case strings.HasSuffix(before, "${"):
			// ${IFS=x} and ${IFS:=x} assign; every other operator only reads.
			unknown = strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, ":=")
		case strings.HasSuffix(before, "$"), unsetBefore.MatchString(before[max(sc.segStart, at-256):]):
		case sc.inArithmetic() || arithAssigns(rest) || strings.HasSuffix(before, "++") || strings.HasSuffix(before, "--") || sc.let || sc.partial(command, at) == "let":
			// (( IFS = 1 )), $(( IFS += 1 )), $[ IFS = 1 ]: an arithmetic value.
			unknown = true
		case strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, "+="):
			rest = rest[strings.IndexByte(rest, '=')+1:]
			value, end, known := ifsWord(rest)
			unknown = !known
			// A prefix to read sets how read splits its input, not the shell's words.
			if after := strings.TrimLeft(rest[end:], " \t"); strings.HasPrefix(after, "read") && (len(after) == 4 || !isNameByte(after[4])) {
				continue
			}
			seps.WriteString(value)
		default:
			// read IFS, printf -v IFS, declare -n x=IFS, for IFS in and the
			// like; a mention as some other command's argument sets nothing.
			named := strings.TrimRight(before, " \t\"'(")
			compared := strings.HasSuffix(named, "==") || strings.HasSuffix(named, "!=")
			unknown = strings.HasSuffix(named, "=") && !compared || sc.setter || ifsSetters[sc.partial(command, at)]
		}
	}
	return seps.String(), unknown
}

// ifsScanner follows the text up to each IFS once, so the scan costs linear
// time however many there are: whether it is inside arithmetic, where the
// simple command and the word around it start, and whether a command
// before it in that simple command sets variables by name, or is let.
type ifsScanner struct {
	pos, segStart, wordStart       int
	open2, close2, openBr, closeBr int
	setter, let                    bool
}

func (sc *ifsScanner) advance(s string, to int) {
	if sc.pos == 0 {
		sc.open2, sc.close2, sc.openBr, sc.closeBr = -1, -1, -1, -1
	}
	for k := sc.pos; k < to; k++ {
		if k+1 < to {
			switch s[k : k+2] {
			case "((":
				sc.open2 = k
			case "))":
				sc.close2 = k
			case "$[":
				sc.openBr = k
			}
		}
		c := s[k]
		if c == ']' {
			sc.closeBr = k
		}
		if strings.IndexByte(" \t\n;&|()`{}", c) < 0 {
			continue
		}
		// A word ends here: read it, then start the next.
		if w := s[sc.wordStart:k]; len(w) < 256 {
			name := path.Base(shellQuotes.Replace(w))
			sc.setter = sc.setter || ifsSetters[name]
			sc.let = sc.let || name == "let"
		}
		sc.wordStart = k + 1
		if c != ' ' && c != '\t' {
			sc.segStart, sc.setter, sc.let = k+1, false, false
		}
	}
	sc.pos = max(sc.pos, to)
}

// partial is the word the text at is inside, up to at, when short enough to
// be a command's name.
func (sc *ifsScanner) partial(s string, at int) string {
	if w := s[sc.wordStart:at]; len(w) < 256 {
		return path.Base(shellQuotes.Replace(w))
	}
	return ""
}

// inArithmetic reports text that ends inside an arithmetic expression,
// ((, $(( or $[, where a name is a variable to read or set.
func (sc *ifsScanner) inArithmetic() bool {
	return sc.open2 > sc.close2 || sc.openBr > sc.closeBr
}

// arithAssigns reports an assignment operator after a name, with blanks
// before it as arithmetic allows: = (not ==), +=, -=, <<= and the rest, ++ or --.
func arithAssigns(rest string) bool {
	if strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, "+=") {
		return false // an assignment as written, read for its value
	}
	rest = strings.TrimLeft(rest, " \t")
	for _, op := range []string{"<<=", ">>=", "+=", "-=", "*=", "/=", "%=", "&=", "^=", "|=", "++", "--"} {
		if strings.HasPrefix(rest, op) {
			return true
		}
	}
	return strings.HasPrefix(rest, "=") && !strings.HasPrefix(rest, "==")
}

// ifsSetters set a variable they are given by name.
var ifsSetters = map[string]bool{
	"read": true, "printf": true, "declare": true, "typeset": true, "local": true, "export": true,
	"readonly": true, "mapfile": true, "readarray": true, "getopts": true, "for": true,
	"select": true, "coproc": true, "wait": true, "let": true,
}

// evalText is text with one layer of quoting removed, as eval would read it:
// quotes dropped, a backslash's character kept and $'...' decoded.
func evalText(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '$' && i+1 < len(text) && text[i+1] == '\'':
			decoded, end := ansiC(text, i+2)
			b.WriteString(decoded)
			i = end
		case c == '$' && i+1 < len(text) && text[i+1] == '"':
		case c == '\'':
			j := strings.IndexByte(text[i+1:], '\'')
			if j < 0 {
				b.WriteString(text[i+1:])
				return b.String()
			}
			b.WriteString(text[i+1 : i+1+j])
			i += j + 1
		case c == '"':
		case c == '\\' && i+1 < len(text):
			b.WriteByte(text[i+1])
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// The shells, and su, flock and script, whose -c runs its next word as a command.
var cShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "ash": true, "su": true, "flock": true, "script": true}

// builtNames reports a word built from an expansion where it may name IFS:
// any word given to eval, a shell's -c text, or the name a setter is given.
// The text eval or -c runs is read again with its quoting undone, so an
// escaped \${v} there counts.
func builtNames(command string) bool { return builtNamesIn(command, 0) }

func builtNamesIn(command string, depth int) bool {
	if depth > 8 {
		return true
	}
	for _, words := range shellWords(command) {
		for i, w := range words {
			name := path.Base(shellQuotes.Replace(w))
			rest := words[i+1:]
			switch {
			case name == "eval":
				for _, a := range rest {
					if expands(a) {
						return true
					}
				}
				if len(rest) > 0 && builtNamesIn(evalText(strings.Join(rest, " ")), depth+1) {
					return true
				}
			case cShells[name]:
				for j, a := range rest {
					if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
						if j+1 < len(rest) && (expands(rest[j+1]) || builtNamesIn(evalText(rest[j+1]), depth+1)) {
							return true
						}
						break
					}
				}
			case ifsSetters[name] && setterNameBuilt(name, rest):
				return true
			}
		}
	}
	return false
}

// setterNameBuilt reports a name built from an expansion among the words a
// setter is given: an option's value that is a name, glued (-vNAME) or the
// next word, or a name given as an operand. Other option values and
// redirections are passed over.
func setterNameBuilt(setter string, args []string) bool {
	names := map[string]string{"printf": "v", "wait": "p", "read": "a"}[setter]
	values := map[string]string{"read": "dinNptu", "mapfile": "dnOsuCc", "readarray": "dnOsuCc"}[setter]
	operands := setter != "printf" && setter != "wait"
	value, name, target := false, false, false
	for i, a := range args {
		if redirect, detached := redirection(a); target || redirect {
			target = redirect && detached
			continue
		}
		switch {
		case name:
			if expands(a) {
				return true
			}
			name = false
		case value:
			value = false
		case setter == "for" || setter == "select":
			return expands(a)
		case strings.HasPrefix(a, "-") && a != "-" && a != "--":
			for j := 1; j < len(a); j++ {
				if strings.IndexByte(names, a[j]) >= 0 {
					if v := a[j+1:]; v != "" && expands(v) {
						return true
					}
					name = a[j+1:] == ""
					break
				}
				if strings.IndexByte(values, a[j]) >= 0 {
					value = a[j+1:] == ""
					break
				}
			}
			// declare -n names its target in the value.
			if strings.Contains(a, "n") && names == "" && values == "" && setter != "getopts" {
				for _, v := range args[i+1:] {
					if expands(v) {
						return true
					}
				}
			}
		case operands:
			n, _, _ := strings.Cut(a, "=")
			if expands(n) {
				return true
			}
		}
	}
	return false
}

// expands reports a word with a $ or backtick the shell expands: not inside
// single quotes or escaped, and not a $'...' string.
func expands(w string) bool {
	single, double := false, false
	for i := 0; i < len(w); i++ {
		c := w[i]
		switch {
		case single:
			single = c != '\''
		case c == '\\':
			i++
		case c == '`':
			return true
		case c == '"':
			double = !double
		case c == '\'' && !double:
			single = true
		case c == '$' && i+1 < len(w) && w[i+1] == '\'' && !double:
			single = true
			i++
		case c == '$' && i+1 < len(w) && (isNameByte(w[i+1]) || strings.IndexByte("{(@*#?!-$", w[i+1]) >= 0):
			return true
		}
	}
	return false
}

// ifsWord reads the value of an assignment from s: the text, where it ends,
// and false when an expansion makes the value unknown.
func ifsWord(s string) (string, int, bool) {
	var b strings.Builder
	known := true
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case strings.IndexByte(" \t\n;&|<>()", c) >= 0:
			return b.String(), i, known
		case c == '\\' && i+1 < len(s):
			b.WriteByte(s[i+1])
			i += 2
		case c == '$' && i+1 < len(s) && s[i+1] == '\'':
			text, end := ansiC(s, i+2)
			b.WriteString(text)
			i = end + 1
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				b.WriteString(s[i+1:])
				return b.String(), len(s), known
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 2
		case c == '"':
			j := i + 1
			for ; j < len(s) && s[j] != '"'; j++ {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				known = known && s[j] != '$' && s[j] != '`'
				b.WriteByte(s[j])
			}
			i = j + 1
		case c == '$' || c == '`':
			known = false
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), min(i, len(s)), known
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
	// A NUL ends the string as bash builds it; the rest up to the quote is dropped.
	nul := func(from int) (string, int) {
		for j := from; j < len(command); j++ {
			switch command[j] {
			case '\\':
				j++
			case '\'':
				return b.String(), j
			}
		}
		return b.String(), len(command)
	}
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
		case 'c':
			// \cX is control-X, as bash 3.2 makes it: \c@ is a NUL, \c? is
			// 0x1f, and \c at the end is a backslash.
			if i+1 >= len(command) || command[i+1] == '\'' {
				b.WriteByte('\\')
				continue
			}
			i++
			x := command[i]
			if x == '\\' && i+1 < len(command) && command[i+1] == '\\' {
				// \c\\ is control-backslash, then the backslash it was paired with.
				b.WriteString("\x1c\\")
				i++
				continue
			}
			if x >= 'a' && x <= 'z' {
				x -= 'a' - 'A'
			}
			c := x & 0x1f
			if c == 0 {
				return nul(i + 1)
			}
			b.WriteByte(c)
		case 'x', 'u', 'U':
			max := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
			j := i + 1
			for j < len(command) && j-i-1 < max && strings.IndexByte("0123456789abcdefABCDEF", command[j]) >= 0 {
				j++
			}
			n, err := strconv.ParseUint(command[i+1:j], 16, 32)
			switch {
			case err != nil:
				b.WriteByte('\\')
				b.WriteByte(e)
				continue
			case n == 0:
				return nul(j)
			case e == 'x':
				// \xHH is a byte, as bash makes it, not a character.
				var v byte
				for _, d := range []byte(command[i+1 : j]) {
					v = v<<4 | hexValue(d)
				}
				b.WriteByte(v)
			default:
				b.WriteRune(runeOf(n))
			}
			i = j - 1
		case '0', '1', '2', '3', '4', '5', '6', '7':
			j := i
			for j < len(command) && j-i < 3 && command[j] >= '0' && command[j] <= '7' {
				j++
			}
			// An octal escape is a byte; bash keeps its low eight bits.
			var v byte
			for _, d := range []byte(command[i:j]) {
				v = v<<3 | (d - '0')
			}
			if v == 0 {
				return nul(j)
			}
			b.WriteByte(v)
			i = j - 1
		default:
			b.WriteByte(e)
		}
	}
	return b.String(), len(command)
}

// hexValue is the value of one hexadecimal digit.
func hexValue(d byte) byte {
	switch {
	case d >= '0' && d <= '9':
		return d - '0'
	case d >= 'a' && d <= 'f':
		return d - 'a' + 10
	}
	return d - 'A' + 10
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
	for _, words := range shellWords(canon) {
		pos := position{on: true}
		for _, w := range words {
			// An assignment, i=$((i+1)) or env A=$B, sets a variable; it is never the program.
			if !pos.next(w) || w == gitSubstitution || isAssignment(w) {
				continue
			}
			if programNamed(w) {
				return "program named by an expansion", true
			}
		}
	}
	return "", false
}

// runeOf is n as a rune, or the replacement character past the last one, as
// WriteRune would write for it anyway.
func runeOf(n uint64) rune {
	if n > unicode.MaxRune {
		return unicode.ReplacementChar
	}
	return rune(n)
}

// ProgramHidden reports a program named by an expansion with an operator,
// such as ${x%/} or ${x//_/ }, whose words no rule can read.
func (c Canonical) ProgramHidden() bool {
	return programOperator(c.Text) || programOperator(c.command) || c.shell.edited
}

// Opaque says why the command runs text no rule can read: a shell reading
// its commands from a pipe or a file, an alias built from an expansion, or
// hash -p or enable -f making a name run something else; "" when none.
func (c Canonical) Opaque() string { return c.shell.opaque }

// Incomplete reports a command that ends before a construct it opened does:
// an unclosed quote, a trailing | or &&, an open loop, block or heredoc.
func (c Canonical) Incomplete() bool { return c.shell.incomplete }

// AsTerminalLine is c read as one line typed at an interactive terminal,
// where the shell may hold lines before it and complete it with lines
// after: a line the parser cannot read on its own (for x in *; do, done, an
// open quote) is left to the text checks rather than refused for that.
func (c Canonical) AsTerminalLine() Canonical {
	if !c.shell.top {
		return c
	}
	c.shell.unparsed, c.shell.incomplete, c.shell.top = c.shell.nested || c.shell.misplaced, false, false
	c.Hidden = c.textHidden || c.shell.tooLong
	c.IFS = blankless(c.IFS)
	c.IFSUnknown = c.textIFSUnknown || c.shell.ifsUnknown || c.shell.unparsed
	c.Hidden = c.Hidden || c.shell.unparsed
	c.IFSSplit = (c.IFS != "" || c.IFSUnknown) && strings.ContainsAny(c.command, "$`")
	return c
}

// Unparsed reports a command a bash parser cannot read, such as one with an
// unclosed quote: where its words start and end is not known.
func (c Canonical) Unparsed() bool { return c.shell.unparsed }

// Commands are the simple commands a bash parser finds, from each word that
// may be the program, quoting undone: inside functions, subshells and the
// literal text eval, trap and a shell's -c run. Deny and ask rules read them.
func (c Canonical) Commands() []string { return c.shell.commands }

// programOperator reports a word in a program's position, not an assignment,
// holding a ${...} with an operator: its value is the text edited at run time.
func programOperator(command string) bool {
	for _, words := range shellWords(command) {
		pos := position{on: true}
		for _, w := range words {
			if pos.next(w) && !isAssignment(w) && operatorExpansion(shellQuotes.Replace(w)) {
				return true
			}
		}
	}
	return false
}

// isAssignment reports a word that sets a variable: a name, then = or +=.
func isAssignment(w string) bool {
	i := 0
	for i < len(w) && isNameByte(w[i]) {
		i++
	}
	return i > 0 && (w[0] < '0' || w[0] > '9') && (strings.HasPrefix(w[i:], "=") || strings.HasPrefix(w[i:], "+="))
}

// operatorExpansion reports a ${...} in w that is more than a name: an
// operator, a length or an indirection, or one not closed in the word.
func operatorExpansion(w string) bool {
	for i := strings.Index(w, "${"); i >= 0; i = strings.Index(w, "${") {
		w = w[i+2:]
		end := strings.IndexByte(w, '}')
		if end < 0 {
			return true
		}
		inner := w[:end]
		if len(inner) == 1 && strings.IndexByte("@*#?-$!0123456789", inner[0]) >= 0 {
			w = w[end+1:]
			continue
		}
		for j := 0; j < len(inner); j++ {
			if !isNameByte(inner[j]) {
				return true
			}
		}
		w = w[end+1:]
	}
	return false
}

// shellWords splits command into the words of each simple command, roughly as
// the shell does: quotes and ${...} keep a word whole, each $(...) is one
// word, a redirection is one word with any target glued to it, and ; & | ( )
// ` and a newline end a command. It is a text check beside the bash parser
// and may only err toward reading more: a comment or a heredoc body is split
// as commands too, which can only add a refusal, so do not skip such text.
func shellWords(command string) [][]string {
	s := collapseSubstitutions(command)
	var parts [][]string
	var words []string
	var w strings.Builder
	word := func() {
		if w.Len() > 0 {
			words = append(words, w.String())
			w.Reset()
		}
	}
	part := func() {
		word()
		if len(words) > 0 {
			parts = append(parts, words)
			words = nil
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			w.WriteString(s[i : i+2])
			i++
		case c == '\'' || c == '"' || c == '$' && i+1 < len(s) && (s[i+1] == '\'' || s[i+1] == '{'):
			end := quoteEnd(s, i)
			w.WriteString(s[i:end])
			i = end - 1
		case c == ' ' || c == '\t' || c == '\r':
			word()
		case c == '<' || c == '>' || c == '&' && strings.HasPrefix(s[i:], "&>"):
			if !fdPrefix(w.String()) {
				word()
			}
			op := redirectOp(s[i:])
			w.WriteString(op)
			i += len(op) - 1
		case strings.IndexByte("\n;&|()`", c) >= 0:
			part()
		default:
			w.WriteByte(c)
		}
	}
	part()
	return parts
}

// quoteEnd returns the index just past the quoted span or ${...} that opens
// at s[i], or len(s) when it is not closed.
func quoteEnd(s string, i int) int {
	switch {
	case s[i] == '\'':
		if j := strings.IndexByte(s[i+1:], '\''); j >= 0 {
			return i + j + 2
		}
	case s[i] == '"' || s[i+1] == '\'':
		quote, j := s[i], i+1
		if s[i] == '$' {
			quote, j = '\'', i+2
		}
		for ; j < len(s); j++ {
			switch s[j] {
			case '\\':
				j++
			case quote:
				return j + 1
			}
		}
	default: // ${
		depth := 0
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '{':
				depth++
			case '}':
				if depth--; depth == 0 {
					return j + 1
				}
			}
		}
	}
	return len(s)
}

// redirectOps are the shell's redirection operators, longest first.
var redirectOps = []string{"&>>", "&>", "<<<", "<<-", "<<", "<>", "<&", ">>", ">|", ">&", "<", ">"}

func redirectOp(s string) string {
	for _, op := range redirectOps {
		if strings.HasPrefix(s, op) {
			return op
		}
	}
	return s[:1]
}

// fdPrefix reports the text before a redirection operator that names its file
// descriptor: digits, or {name}.
func fdPrefix(w string) bool {
	if strings.HasPrefix(w, "{") && strings.HasSuffix(w, "}") && len(w) > 2 {
		for i := 1; i < len(w)-1; i++ {
			if !isNameByte(w[i]) {
				return false
			}
		}
		return true
	}
	return w != "" && strings.Trim(w, "0123456789") == ""
}

// redirection reports a word that is a redirection, such as 2>/dev/null, <f
// or &>>log, and whether its target is the next word, as in > f.
func redirection(w string) (bool, bool) {
	i := 0
	if strings.HasPrefix(w, "{") {
		if j := strings.IndexByte(w, '}'); j > 0 && fdPrefix(w[:j+1]) {
			i = j + 1
		}
	}
	for i < len(w) && w[i] >= '0' && w[i] <= '9' {
		i++
	}
	if i == len(w) || w[i] != '<' && w[i] != '>' && !strings.HasPrefix(w[i:], "&>") {
		return false, false
	}
	return true, w[i:] == redirectOp(w[i:])
}

// programNamed reports a program's word with an expansion whose value may name
// the program or split into its words: any $ or backtick but $$, $?, $#, $! and
// $-, and a plain one inside double quotes followed by a /, which can only be
// a directory, as in "$(pwd)"/run.sh or "$HOME/bin/tool". The whole word is
// read, so a / inside ${x%/} is not taken for a directory.
func programNamed(w string) bool {
	double := false
	for i := 0; i < len(w); i++ {
		c := w[i]
		switch {
		case c == '\\':
			i++
		case c == '`':
			return true
		case c == '"':
			double = !double
		case c == '\'' && !double:
			i = quoteEnd(w, i) - 1
		case c != '$' || i+1 == len(w):
		case w[i+1] == '\'' && !double:
			i = quoteEnd(w, i) - 1
		case w[i+1] == '"':
			// $"..." is a translated string, quoted as "..." is.
		case strings.IndexByte("$?#!-", w[i+1]) >= 0:
			i++
		default:
			end := expansionEnd(w, i)
			if end == i+1 {
				continue // a lone $ is itself
			}
			// $IFS is read as the blank it is, in the canonical text.
			if x := w[i:end]; x == "$IFS" || x == "${IFS}" {
				i = end - 1
				continue
			}
			// "$@" and "$*" are a word per parameter, never only a directory.
			if x := w[i:end]; x == "$@" || x == "${@}" || x == "$*" || x == "${*}" {
				return true
			}
			rest := w[end:]
			if !double || strings.HasPrefix(w[i:], "${") && operatorExpansion(w[i:end]) ||
				!strings.HasPrefix(rest, "/") && !strings.HasPrefix(rest, "\"/") {
				return true
			}
			i = end - 1
		}
	}
	return false
}

// expansionEnd returns the index just past the expansion whose $ is at w[i]:
// a name, a digit or special parameter, or ${...}; i+1 when it is none.
func expansionEnd(w string, i int) int {
	j := i + 1
	switch {
	case w[j] == '{':
		return quoteEnd(w, i)
	case w[j] == '_' || w[j] >= 'a' && w[j] <= 'z' || w[j] >= 'A' && w[j] <= 'Z':
		for j < len(w) && isNameByte(w[j]) {
			j++
		}
		return j
	case w[j] >= '0' && w[j] <= '9' || w[j] == '@' || w[j] == '*':
		return j + 1
	}
	return i + 1
}
