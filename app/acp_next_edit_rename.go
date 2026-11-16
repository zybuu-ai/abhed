package app

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// A rename's next edit (studio-acp-contract.md §7.11): when the person's last
// edit turned one identifier into another, the other uses of the old name
// are the next edit. They are found in the document itself, from the sites
// the editor's language server named or else by a scan of the enclosing
// function, and offered as one edit; no model is asked and nothing is sent.
// The scan runs only for languages whose strings, comments and reserved
// words it knows, and refuses what it cannot be sure of: a name bound again
// in a nested scope, or a new name already used there.

// renameHint is Studio's reading of the last edit as a rename: From became
// To, which now starts at At.
type renameHint struct {
	From string  `json:"from"`
	To   string  `json:"to"`
	At   editPos `json:"at"`
}

// keywords are each language's reserved words (and, for Go, its predeclared
// constants), which a rename may not produce. A language not here gets no
// rename from the file.
var keywords = map[string]string{
	"python":     "False None True and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield",
	"go":         "break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var nil true false iota",
	"javascript": "await break case catch class const continue debugger default delete do else enum export extends false finally for function if implements import in instanceof interface let new null package private protected public return static super switch this throw true try typeof var void while with yield",
	"typescript": "await break case catch class const continue debugger default delete do else enum export extends false finally for function if implements import in instanceof interface let new null package private protected public return static super switch this throw true try typeof var void while with yield",
	"java":       "abstract assert boolean break byte case catch char class const continue default do double else enum extends final finally float for goto if implements import instanceof int interface long native new package private protected public return short static strictfp super switch synchronized this throw throws transient try void volatile while true false null var",
	"rust":       "as async await break const continue crate dyn else enum extern false fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait true type unsafe use where while",
	"c":          "auto break case char const continue default do double else enum extern float for goto if inline int long register restrict return short signed sizeof static struct switch typedef union unsigned void volatile while _Bool _Complex _Imaginary",
	"cpp":        "alignas alignof and asm auto bool break case catch char class const constexpr const_cast continue decltype default delete do double dynamic_cast else enum explicit export extern false float for friend goto if inline int long mutable namespace new noexcept not nullptr operator or private protected public register reinterpret_cast return short signed sizeof static static_assert static_cast struct switch template this thread_local throw true try typedef typeid typename union unsigned using virtual void volatile while xor",
}

// scanned are the languages the file is scanned in when no language server
// names the sites; any other language with keywords takes only those sites.
// JSX (javascriptreact, typescriptreact) is not: an attribute name and the
// text between tags read as names a scan would rename.
var scanned = map[string]bool{"python": true, "go": true, "javascript": true, "typescript": true, "java": true}

func init() {
	keywords["javascriptreact"] = keywords["javascript"]
	keywords["typescriptreact"] = keywords["typescript"]
}

// identStart and identPart are what an identifier is made of.
func identStart(r rune) bool { return r == '_' || r == '$' || unicode.IsLetter(r) }
func identPart(r rune) bool  { return identStart(r) || unicode.IsDigit(r) }

// isIdent reports an identifier that is not one of language's reserved words.
func isIdent(s, language string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for i, r := range s {
		if (i == 0 && !identStart(r)) || !identPart(r) {
			return false
		}
	}
	for _, k := range strings.Fields(keywords[language]) {
		if k == s {
			return false
		}
	}
	return true
}

// byteAt is the byte offset in line of a UTF-16 character offset, or -1.
func byteAt(line string, u16 int) int {
	n := 0
	for i, r := range line {
		if n == u16 {
			return i
		}
		n += utf16.RuneLen(r)
	}
	if n == u16 {
		return len(line)
	}
	return -1
}

// u16Len is line's length in UTF-16 code units, as positions count it.
func u16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// wordAt reports whether line holds exactly word at byte i, not part of a longer identifier.
func wordAt(line string, i int, word string) bool {
	if i < 0 || !strings.HasPrefix(line[i:], word) {
		return false
	}
	if r, _ := utf8.DecodeLastRuneInString(line[:i]); i > 0 && identPart(r) {
		return false
	}
	if r, _ := utf8.DecodeRuneInString(line[i+len(word):]); i+len(word) < len(line) && identPart(r) {
		return false
	}
	return true
}

// wordsIn are the byte offsets of word, whole, in s.
func wordsIn(s, word string) []int {
	var out []int
	for i := 0; i <= len(s)-len(word); {
		j := strings.Index(s[i:], word)
		if j < 0 {
			break
		}
		if wordAt(s, i+j, word) {
			out = append(out, i+j)
		}
		i += j + len(word)
	}
	return out
}

// renameOf is the rename the request describes, Studio's hint first, else
// its last recent edit in this document read as one: an identifier replaced
// by another, the new one in the window where the edit says. line is the
// rename's line within the window. A language with no reserved words here
// has none; one the scan does not know has one only with the language
// server's sites.
func renameOf(req nextEditRequest, lines []string) (h renameHint, line int, ok bool) {
	if _, known := keywords[req.Language]; !known || (!scanned[req.Language] && len(req.Sites) == 0) {
		return h, 0, false
	}
	if req.Rename != nil {
		h = *req.Rename
	} else if n := len(req.RecentEdits); n > 0 {
		e := req.RecentEdits[n-1]
		if !sameFileURI(e.URI, req.URI) || e.Range.Start.Line != e.Range.End.Line {
			return h, 0, false
		}
		h = renameHint{From: e.OldText, To: e.NewText, At: e.Range.Start}
	} else {
		return h, 0, false
	}
	if h.From == h.To || !isIdent(h.From, req.Language) || !isIdent(h.To, req.Language) {
		return h, 0, false
	}
	line = h.At.Line - req.Window.StartLine
	if line < 0 || line >= len(lines) || !wordAt(lines[line], byteAt(lines[line], h.At.Character), h.To) {
		return h, 0, false
	}
	return h, line, true
}

// sameFileURI is whether two file URIs are one document as the editor sends them.
func sameFileURI(a, b string) bool { return a == b }

// codeOnly is the lines with their strings and comments blanked, byte for
// byte, so a scan finds only code: Python's # and quotes (triple ones over
// lines), and the C family's //, /* */, quotes, backticks (Go, JavaScript)
// and Java's text blocks.
func codeOnly(lines []string, language string) []string {
	py := language == "python"
	out := make([]string, len(lines))
	var open string // the string or comment a line leaves open: `"""`, `'''`, "*/", "`"
	for li, line := range lines {
		b := []byte(line)
		for i := 0; i < len(b); {
			if open != "" {
				if strings.HasPrefix(line[i:], open) {
					for k := range open {
						b[i+k] = ' '
					}
					i += len(open)
					open = ""
					continue
				}
				if b[i] != '\n' && b[i] != '\r' {
					b[i] = ' '
				}
				i++
				continue
			}
			rest := line[i:]
			switch {
			case (py && (strings.HasPrefix(rest, `"""`) || strings.HasPrefix(rest, `'''`))) || (language == "java" && strings.HasPrefix(rest, `"""`)):
				open = rest[:3]
				b[i], b[i+1], b[i+2] = ' ', ' ', ' '
				i += 3
			case !py && strings.HasPrefix(rest, "/*"):
				open = "*/"
				b[i], b[i+1] = ' ', ' '
				i += 2
			case !py && rest[0] == '`':
				open = "`"
				b[i] = ' '
				i++
			case (py && rest[0] == '#') || (!py && strings.HasPrefix(rest, "//")):
				for ; i < len(b) && b[i] != '\n' && b[i] != '\r'; i++ {
					b[i] = ' '
				}
			case rest[0] == '"' || rest[0] == '\'':
				q := rest[0]
				b[i] = ' '
				i++
				for ; i < len(b) && b[i] != '\n'; i++ {
					c := b[i]
					b[i] = ' '
					if c == '\\' && i+1 < len(b) && b[i+1] != '\n' {
						i++
						b[i] = ' '
						continue
					}
					if c == q {
						i++
						break
					}
				}
			default:
				i++
			}
		}
		out[li] = string(b)
	}
	return out
}

// Function headers, by language, as their code reads with strings and comments blanked.
var (
	pyDef       = regexp.MustCompile(`^\s*(?:async\s+)?def\s`)
	goFunc      = regexp.MustCompile(`\bfunc\b`)
	jsFunc      = regexp.MustCompile(`\bfunction\b|=>\s*\{\s*$`)
	methodShape = regexp.MustCompile(`^\s*(?:[\w$<>\[\],.?@]+\s+)*[\w$#]+\s*(?:<[^>]*>)?\s*\([^;]*\)\s*(?::\s*[^{=;]+)?(?:throws\s+[\w., ]+)?\{\s*$`)
	javaLambda  = regexp.MustCompile(`->\s*\{\s*$`)
	notMethod   = regexp.MustCompile(`^\s*(?:\}\s*)?(?:if|for|while|switch|catch|else|do|try|synchronized|return|new|class|interface|enum|record)\b`)
)

// funcHeader reports a line (code only) that opens a function, method or lambda body.
func funcHeader(code, language string) bool {
	switch language {
	case "python":
		return pyDef.MatchString(code)
	case "go":
		return goFunc.MatchString(code)
	case "javascript", "javascriptreact", "typescript", "typescriptreact":
		return jsFunc.MatchString(code) || (methodShape.MatchString(code) && !notMethod.MatchString(code))
	case "java":
		return javaLambda.MatchString(code) || (methodShape.MatchString(code) && !notMethod.MatchString(code))
	}
	return false
}

func indentOf(s string) int   { return len(s) - len(strings.TrimLeft(s, " \t")) }
func blankLine(s string) bool { return strings.TrimSpace(s) == "" }

// blockEnd is where the block a header at line h opens ends: the next line
// back at its indentation, a closing line at it included.
func blockEnd(lines []string, h int) int {
	for i := h + 1; i < len(lines); i++ {
		if !blankLine(lines[i]) && indentOf(lines[i]) <= indentOf(lines[h]) {
			if t := strings.TrimSpace(lines[i]); strings.HasPrefix(t, "}") || strings.HasPrefix(t, ")") {
				return i + 1
			}
			return i
		}
	}
	return len(lines)
}

// scopeOf is the window's lines a rename at line can reach, [from, to): the
// innermost function it is in (the function itself when line is its
// header). A rename at the top level of the file reaches the window; one
// in another block with no function the window shows (a class body, say)
// reaches nothing, ok false.
func scopeOf(lines, code []string, line int, language string) (int, int, bool) {
	if funcHeader(code[line], language) {
		return line, blockEnd(lines, line), true
	}
	ind := indentOf(lines[line])
	if ind == 0 {
		return 0, len(lines), true
	}
	for i := line - 1; i >= 0; i-- {
		if blankLine(lines[i]) || indentOf(lines[i]) >= ind {
			continue
		}
		if funcHeader(code[i], language) {
			return i, blockEnd(lines, i), true
		}
		ind = indentOf(lines[i])
		if ind == 0 {
			break
		}
	}
	return 0, 0, false
}

// boundAgain reports a line of code that binds name anew in its own scope:
// a parameter, a declaration, a loop or comprehension variable, a lambda's.
func boundAgain(code, name, language string) bool {
	q := regexp.QuoteMeta(name)
	var pats []string
	switch language {
	case "python":
		pats = []string{`\blambda\b[^:]*\b` + q + `\b`, `\S.*\bfor\s+(?:[\w\s,()]*,\s*)?\(?` + q + `\b.*\bin\b`}
	case "go":
		pats = []string{`\b` + q + `\b[\w\s,]*:=`, `\bvar\s+` + q + `\b`}
	case "javascript", "javascriptreact", "typescript", "typescriptreact":
		pats = []string{`\b(?:let|const|var)\s+(?:[\w$\s,{\[]*[,{\[]\s*)?` + q + `\b`, `\b` + q + `\s*=>`, `\(\s*(?:[\w$\s,]*,\s*)?` + q + `\s*[,)][^)]*\)\s*=>`}
	case "java":
		// A type before the name declares it; a keyword before it (return, throw, case) does not.
		for _, m := range regexp.MustCompile(`\b([A-Za-z_][\w<>\[\].?]*)\s+`+q+`\s*[=;:,)]`).FindAllStringSubmatch(code, -1) {
			switch m[1] {
			case "return", "throw", "case", "new", "else", "yield", "assert", "instanceof":
			default:
				return true
			}
		}
		pats = []string{`\b` + q + `\s*->`}
	}
	for _, p := range pats {
		if regexp.MustCompile(p).MatchString(code) {
			return true
		}
	}
	return false
}

// Where the old name is bound in a rename's scope, apart from the rename itself.
const (
	notBound = iota
	// boundNested: bound again in a nested function, lambda, comprehension
	// or (outside Python) inner block, where a textual rename would change
	// another variable. The rename is left to the person.
	boundNested
	// boundHere: declared in the scope itself, outside Python (where a
	// variable is declared once): the edit was at a use, not the
	// declaration, so it is not a rename to finish.
	boundHere
)

// bindingOf says where name is bound in [from, to) other than at line.
func bindingOf(lines, code []string, from, to, line int, name, language string) int {
	for i := from; i < to; i++ {
		if i == line {
			continue
		}
		nested := i > from && funcHeader(code[i], language)
		if nested && len(wordsIn(code[i], name)) > 0 {
			return boundNested // a parameter, or a use it may shadow, on a nested header
		}
		if language == "python" {
			// Python has no block scope: only lambdas, comprehensions and nested defs bind apart.
			if boundAgain(code[i], name, language) {
				return boundNested
			}
			if nested {
				for j := i + 1; j < blockEnd(lines, i); j++ {
					if regexp.MustCompile(`^\s*` + regexp.QuoteMeta(name) + `\s*(?:[+\-*/%&|^]|//|\*\*)?=[^=]|\b(?:for|as|global|nonlocal)\s+` + regexp.QuoteMeta(name) + `\b`).MatchString(code[j]) {
						return boundNested
					}
				}
			}
			continue
		}
		if boundAgain(code[i], name, language) {
			if indentOf(lines[i]) > indentOf(lines[line]) {
				return boundNested
			}
			return boundHere
		}
	}
	return notBound
}

// blockWords open a statement whose brace is a block, not an object literal.
var blockWords = regexp.MustCompile(`^\s*(?:\}\s*)?(?:if|else|for|while|do|switch|select|case|default|try|catch|finally|func|function|class|interface|struct|enum|namespace|go|defer|with)\b`)

// goMapType ends a Go map literal's type, whose keys are expressions, not
// field names; goMapOfMaps one whose values are maps too.
var (
	goMapType   = regexp.MustCompile(`\bmap\[[^\]]*\][\w.*\[\]]*$`)
	goMapOfMaps = regexp.MustCompile(`\bmap\[[^\]]*\]map\[[^\]]*\][\w.*\[\]]*$`)
)

func jsxLanguage(language string) bool {
	return language == "javascriptreact" || language == "typescriptreact"
}

// braceKind is what a { at the end of code opens: 'o' an object or
// composite literal, whose keys are names (after an operator or opening
// punctuation, after return, or in Go after a type name on a line that is
// not a statement keyword's); 'm' a Go map literal, whose keys are
// expressions ('M' one whose values are maps too); 'x' an expression
// container, a template's ${ or, in JSX, the { of an attribute's value
// (name={), a child (after > or }) or text; '{' a block (after ), => or a
// name elsewhere).
func braceKind(code, language string) byte {
	t := strings.TrimRight(code, " \t")
	if t == "" || strings.HasSuffix(t, "=>") {
		return '{' // a block, or an arrow function's body
	}
	if language == "go" && goMapOfMaps.MatchString(t) {
		return 'M'
	}
	if language == "go" && goMapType.MatchString(t) {
		return 'm'
	}
	if strings.ContainsRune("=(,:[?+-*/%&|^!<>~", rune(t[len(t)-1])) || strings.HasSuffix(t, "return") {
		return 'o' // an operand: an object or composite literal
	}
	if language == "go" && !blockWords.MatchString(code) {
		if r, _ := utf8.DecodeLastRuneInString(t); identPart(r) || r == ']' {
			return 'o'
		}
	}
	return '{'
}

// nextCode is the first character of code after byte at on line l, looking
// on over later lines before end when the line has none after it.
func nextCode(code []string, l, at, end int) byte {
	t := strings.TrimLeft(code[l][at:], " \t\r\n")
	for t == "" && l+1 < end {
		l++
		t = strings.TrimLeft(code[l], " \t\r\n")
	}
	if t == "" {
		return 0
	}
	return t[0]
}

// shorthand reports a JavaScript or TypeScript shorthand property, { name }
// or { a, name }: a name, in code (not in a string or template), right after
// { or , and before , or } in an object literal or destructuring ('o'), not
// in a block, a template's ${ or a JSX expression container. It is both the
// key and the variable, so a rename writes it out as name: newName.
func shorthand(code []string, l, at, first, end int, name string, open byte, language string) bool {
	return open == 'o' && shorthandShape(code, l, at, first, end, name, language)
}

// shorthandShape is a JavaScript or TypeScript name, in code, that reads
// as shorthand or a key wherever it is: right after { or , and before , }
// or :.
func shorthandShape(code []string, l, at, first, end int, name string, language string) bool {
	if !jsFamily(language) || !strings.HasPrefix(code[l][at:], name) {
		return false
	}
	p, n := prevCode(code, l, at, first), nextCode(code, l, at+len(name), end)
	return (p == '{' || p == ',') && (n == ',' || n == '}')
}

// unsure reports a use the rename cannot classify: shorthand-shaped in a
// brace jsBraceKind could not tell ('u'), or, in a template's ${ … } that
// the scan reads as text, after { or , other than the ${ itself and before
// , or }. The rename is then left to the model.
func unsure(lines, code []string, l, at, first, end int, name string, open byte, language string) bool {
	if !jsFamily(language) {
		return false
	}
	if strings.HasPrefix(code[l][at:], name) {
		return open == 'u' && shorthandShape(code, l, at, first, end, name, language)
	}
	// Blanked: in a string or template. Read the raw line around it.
	before := strings.TrimRight(lines[l][:at], " \t")
	after := strings.TrimLeft(lines[l][at+len(name):], " \t")
	if before == "" || after == "" {
		return false
	}
	p, n := before[len(before)-1], after[0]
	if p == '{' && strings.HasSuffix(before, "${") {
		return false // `${name}`: a plain use
	}
	return (p == '{' || p == ',') && (n == ',' || n == '}')
}

// prevCode is the last character of code before byte at on line l, looking
// back over earlier lines from first when the line has none before it.
func prevCode(code []string, l, at, first int) byte {
	t := strings.TrimRight(code[l][:at], " \t")
	for t == "" && l > first {
		l--
		t = strings.TrimRight(code[l], " \t\r\n")
	}
	if t == "" {
		return 0
	}
	return t[len(t)-1]
}

// notAVariable reports a use at byte at of code[l] that names something
// else: a member (after `.`, `->` or `::`); a keyword argument (followed by
// `=` inside call parentheses), except on the scope's own header (first),
// where it is a parameter's default; or, in JavaScript, TypeScript and Go,
// a key (right after `{` or `,` and followed by `:`) inside a brace that
// opened an expression ('o' as open), never a ternary's `:` or a case's.
func notAVariable(code []string, l, at, first int, name string, open byte, language string) bool {
	before := strings.TrimRight(code[l][:at], " \t")
	if strings.HasSuffix(before, ".") || strings.HasSuffix(before, "->") || strings.HasSuffix(before, "::") {
		return true
	}
	after := strings.TrimLeft(code[l][at+len(name):], " \t")
	if open == '(' && l != first && strings.HasPrefix(after, "=") && !strings.HasPrefix(after, "==") && !strings.HasPrefix(after, "=>") {
		return true
	}
	if language != "python" && language != "java" && open == 'o' && strings.HasPrefix(after, ":") && !strings.HasPrefix(after, "::") {
		if p := prevCode(code, l, at, first); p == '{' || p == ',' {
			return true
		}
	}
	return false
}

func jsFamily(language string) bool {
	return strings.HasPrefix(language, "javascript") || strings.HasPrefix(language, "typescript")
}

// JavaScript and TypeScript brace contexts.
var (
	jsDeclEnd    = regexp.MustCompile(`(?:^|[^\w$.])(?:const|let|var|using|import|export)$`)
	jsBlockWord  = regexp.MustCompile(`(?:^|[^\w$.])(?:else|try|finally|do)$`)
	jsTypeHeader = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?(?:declare\s+)?(?:class|interface|enum|namespace|module)\b`)
	jsCaseLine   = regexp.MustCompile(`^\s*(?:case\b.*|default\s*):$`)
	jsxTagOpen   = regexp.MustCompile(`<[A-Za-z][\w.:-]*(?:\s[^<>]*)?$`)
	jsxAttrEq    = regexp.MustCompile(`[\w$-]=$`)
)

// jsBraceKind is braceKind for JavaScript and TypeScript, where a brace
// decides whether a name is shorthand, so it answers only when it is sure:
// 'o' an object literal or destructuring, 'x' an expression container (a
// template's ${, a JSX child or attribute value), '{' a block, a body or a
// type's members, and 'u' when it cannot tell, which leaves a shorthand-
// shaped use there to the model. before is the line up to the brace, prev
// the last character of code before it (across lines), top the innermost
// bracket already open.
func jsBraceKind(before string, prev, top byte, language string) byte {
	t := strings.TrimRight(before, " \t")
	jsx := jsxLanguage(language)
	switch {
	case strings.HasSuffix(before, "$"):
		return 'x' // ${ in a template
	case top == 'x' && prev == '{':
		return 'o' // {{ … }}: an object inside a JSX expression container
	case strings.HasSuffix(t, "=>"):
		return '{' // an arrow function's body
	case t == "":
		// The brace starts its line: the line before decides.
		switch {
		case prev != 0 && strings.ContainsRune("([,", rune(prev)):
			return 'o'
		case prev == ')':
			return '{'
		}
		return 'u'
	}
	last := t[len(t)-1]
	switch {
	case jsx && (last == '>' || last == '}'):
		return 'x' // a JSX child
	case jsx && last == '=' && jsxTagOpen.MatchString(t):
		return 'x' // an attribute's value: <Item name = {
	case jsx && last == '=' && jsxAttrEq.MatchString(t) && !strings.HasSuffix(t, " =") && !jsDeclLine.MatchString(t):
		return 'x' // name={ in a tag begun on an earlier line
	case jsx && last == '=' && !jsDeclLine.MatchString(t):
		return 'u' // = { in JSX with no tag or declaration in sight: an attribute or an assignment
	case strings.ContainsRune("([,", rune(last)):
		return 'o' // an argument, element or parameter: an object or destructuring
	case last == ':' && jsCaseLine.MatchString(t):
		return '{' // case x: { … }
	case strings.ContainsRune("=:?+-*/%&|^!<~", rune(last)) || strings.HasSuffix(t, "return") || strings.HasSuffix(t, "yield") || strings.HasSuffix(t, "await"):
		return 'o' // an operand: an object literal
	case jsDeclEnd.MatchString(t):
		return 'o' // const { … } = o: a destructuring
	case last == ')' || jsBlockWord.MatchString(t):
		return '{' // a block: if (…) {, function f() {, else {, try {
	case jsTypeHeader.MatchString(t):
		return '{' // a class's, interface's or enum's members
	}
	return 'u'
}

// jsDeclLine is a line declaring a name before an =, so its = { is an object.
var jsDeclLine = regexp.MustCompile(`(?:^|[^\w$.])(?:const|let|var|using|type)\s[^=]*=$|(?:^|[^\w$.])(?:readonly|private|public|protected|static)\s[^=]*=$`)

// bracketPass walks code over lines [from, to) with a stack of open
// brackets, a { by its braceKind, and calls visit at every byte with the
// innermost one open there (0 for none).
func bracketPass(code []string, from, to int, language string, visit func(l, i int, open byte)) {
	var stack []byte
	top := func() byte {
		if len(stack) == 0 {
			return 0
		}
		return stack[len(stack)-1]
	}
	for l := from; l < to; l++ {
		c := code[l]
		for i := 0; i < len(c); i++ {
			visit(l, i, top())
			switch c[i] {
			case '{':
				var kind byte
				if jsFamily(language) {
					kind = jsBraceKind(c[:i], prevCode(code, l, i, from), top(), language)
				} else {
					kind = braceKind(c[:i], language)
				}
				// A map of maps' value with its type left out is a map literal too.
				if top() == 'M' && kind == 'o' && prevCode(code, l, i, from) == ':' {
					kind = 'm'
				}
				stack = append(stack, kind)
			case '(', '[':
				stack = append(stack, c[i])
			case ')', ']', '}':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
		}
	}
}

// renameEdit is the one edit that finishes a rename in the window: every
// other use of h.From, at the sites the language server named when it named
// any here, else as found by a scan of the rename's scope. Lines are the
// window as Studio sent it (none of it goes anywhere). why says why there is
// none; notRename that the edit is not taken for a rename after all, and
// the model is asked as for any other edit.
func renameEdit(req nextEditRequest, lines []string, h renameHint, line int) (edits []suggestedEdit, why string, notRename bool) {
	type site struct {
		line, at int  // window line, byte offset
		short    bool // a shorthand property: written out as name: newName
	}
	var sites []site
	code := codeOnly(lines, req.Language)
	from, to, inFunc := scopeOf(lines, code, line, req.Language)
	if !inFunc && len(req.Sites) == 0 {
		return nil, "the rename is not in a function this window shows", true
	}
	if !inFunc {
		from, to = 0, len(lines)
	}
	// A real rename brings in a name that was not there: one already used
	// beside it may be another variable, and is left to the person.
	for l := from; l < to; l++ {
		for _, at := range wordsIn(code[l], h.To) {
			if l != line || at != byteAt(lines[line], h.At.Character) {
				return nil, h.To + " is already used here, so this is not taken for a rename", true
			}
		}
	}
	if len(req.Sites) > 0 {
		// The brackets open at each site, read as the scan reads them.
		opens := map[[2]int]byte{}
		bracketPass(code, 0, len(lines), req.Language, func(l, i int, open byte) { opens[[2]int{l, i}] = open })
		for _, r := range req.Sites {
			l := r.Start.Line - req.Window.StartLine
			if r.Start.Line != r.End.Line || l < 0 || l >= len(lines) {
				continue
			}
			at := byteAt(lines[l], r.Start.Character)
			if at >= 0 && byteAt(lines[l], r.End.Character) == at+len(h.From) && wordAt(lines[l], at, h.From) {
				open := opens[[2]int{l, at}]
				if unsure(lines, code, l, at, 0, len(lines), h.From, open, req.Language) {
					return nil, "a use of " + h.From + " may be an object's key, so this is left to the model", true
				}
				// A shorthand property the server names is written out too, so its key stays.
				sites = append(sites, site{l, at, shorthand(code, l, at, 0, len(lines), h.From, open, req.Language)})
			}
		}
	} else {
		switch bindingOf(lines, code, from, to, line, h.From, req.Language) {
		case boundNested:
			return nil, h.From + " is bound again in a nested scope here, so the rename is left to the person", false
		case boundHere:
			return nil, h.From + " is declared elsewhere in this function: the edit was at a use, not a rename", true
		}
		uses := map[[2]int]bool{}
		for l := from; l < to; l++ {
			for _, i := range wordsIn(code[l], h.From) {
				uses[[2]int{l, i}] = true
			}
		}
		unsureUse := false
		bracketPass(code, from, to, req.Language, func(l, i int, open byte) {
			if !uses[[2]int{l, i}] {
				return
			}
			if unsure(lines, code, l, i, from, to, h.From, open, req.Language) {
				unsureUse = true
			} else if !notAVariable(code, l, i, from, h.From, open, req.Language) {
				sites = append(sites, site{l, i, shorthand(code, l, i, from, to, h.From, open, req.Language)})
			}
		})
		if unsureUse {
			return nil, "a use of " + h.From + " may be an object's key, so this is left to the model", true
		}
	}
	slices.SortFunc(sites, func(a, b site) int { return cmp.Or(a.line-b.line, a.at-b.at) })
	sites = slices.Compact(sites)
	if len(sites) == 0 {
		// Nothing to finish: a one-word edit (a typo fixed, another name used) is no rename.
		return nil, "no other use of " + h.From + " was found near the rename", true
	}
	first, last := sites[0].line, sites[0].line
	for _, s := range sites {
		first, last = min(first, s.line), max(last, s.line)
	}
	changed := append([]string(nil), lines[first:last+1]...)
	// Right to left within a line, so earlier offsets stay true.
	for i := len(sites) - 1; i >= 0; i-- {
		s := sites[i]
		l := changed[s.line-first]
		to := h.To
		if s.short {
			to = h.From + ": " + h.To
		}
		changed[s.line-first] = l[:s.at] + to + l[s.at+len(h.From):]
	}
	end := editPos{Line: req.Window.StartLine + last + 1}
	if !strings.HasSuffix(lines[last], "\n") {
		end = editPos{Line: req.Window.StartLine + last, Character: u16Len(lines[last])}
	}
	return []suggestedEdit{{
		URI:     req.URI,
		Range:   editRange{Start: editPos{Line: req.Window.StartLine + first}, End: end},
		NewText: strings.Join(changed, ""),
	}}, "", false
}

// jsxIn matches JSX in JavaScript code (strings and comments blanked): a
// closing tag, a self-closing end, a fragment; a tag where an operand goes
// (after return, yield, await, =, (, [, ,, :, ?, !, &&, ||, {, }, > or at a
// line's start); or, anywhere, a tag shaped as one, <Name followed by an
// attribute (name=), a spread ({...) or >, or a capitalised <Name ending
// its line (its attributes on the lines after), which no comparison reads as.
// A false match only sends the edit to a language server's sites or the model.
var jsxIn = regexp.MustCompile(`</[A-Za-z>]|/>|<>|(?:^|[=(\[,:?!&|{}>]|\b(?:return|yield|await))\s*<[A-Za-z][\w.:-]*(?:\s|>|/|$)|<[A-Za-z][\w.:-]*(?:\s+[\w$-]+\s*=|\s+\{\s*\.\.\.|>)|<[A-Z][\w.:-]*\s*$`)

// renameLanguage is the language a rename reads the window as: a
// javascript window holding JSX, as editors name a .js file with JSX, is
// javascriptreact, so it is not scanned (JSX text and attribute names read
// as names) and a language server's sites follow the JSX rules. TypeScript
// cannot hold JSX outside .tsx, and its generics (Array<T>) read like tags,
// so it is left as it is.
func renameLanguage(language string, lines []string) string {
	if language != "javascript" {
		return language
	}
	for _, c := range codeOnly(lines, language) {
		if jsxIn.MatchString(c) {
			return "javascriptreact"
		}
	}
	return language
}
