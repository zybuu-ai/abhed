package ui

import (
	"strings"
	"unicode"
)

// hlState is what highlighting carries from one line of a code block to the
// next: an open block comment or multi-line string.
type hlState struct {
	inComment bool   // inside /* ... */ or similar
	inString  string // the delimiter of an open multi-line string
}

// langSpec is enough of a language to colour it: its keywords, its comment
// markers and its string quotes.
type langSpec struct {
	keywords   map[string]bool
	types      map[string]bool
	line       []string // line comment markers
	blockOpen  string
	blockClose string
	quotes     string
	triple     bool // Python's """ and '''
}

func words(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

var cLike = "if else for while do switch case default break continue return goto sizeof static const struct union enum typedef extern volatile inline"

var langs = map[string]*langSpec{
	"go": {keywords: words("break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var nil true false iota"),
		types: words("string int int8 int16 int32 int64 uint uint8 uint16 uint32 uint64 uintptr byte rune float32 float64 complex64 complex128 bool error any"),
		line:  []string{"//"}, blockOpen: "/*", blockClose: "*/", quotes: "\"'`"},
	"python": {keywords: words("and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield None True False self"),
		types: words("int str float bool list dict set tuple bytes object"), line: []string{"#"}, quotes: "\"'", triple: true},
	"javascript": {keywords: words("async await break case catch class const continue debugger default delete do else export extends finally for from function if import in instanceof let new of return static super switch this throw try typeof var void while with yield null undefined true false"),
		types: words("string number boolean any unknown never void object interface type enum implements readonly"), line: []string{"//"}, blockOpen: "/*", blockClose: "*/", quotes: "\"'`"},
	"rust": {keywords: words("as async await break const continue crate dyn else enum extern false fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait true type unsafe use where while"),
		types: words("i8 i16 i32 i64 i128 isize u8 u16 u32 u64 u128 usize f32 f64 bool char str String Vec Option Result Box"), line: []string{"//"}, blockOpen: "/*", blockClose: "*/", quotes: "\""},
	"java": {keywords: words("abstract assert break case catch class const continue default do else enum extends final finally for if implements import instanceof interface native new package private protected public return static super switch synchronized this throw throws try void volatile while null true false var record"),
		types: words("int long short byte char boolean float double String Object"), line: []string{"//"}, blockOpen: "/*", blockClose: "*/", quotes: "\"'"},
	"c": {keywords: words(cLike + " class namespace template typename public private protected virtual override new delete this nullptr true false using auto"),
		types: words("int long short char float double void bool unsigned signed size_t"), line: []string{"//"}, blockOpen: "/*", blockClose: "*/", quotes: "\"'"},
	"shell": {keywords: words("if then else elif fi for while until do done case esac in function return local export readonly unset shift exit source echo cd set"),
		line: []string{"#"}, quotes: "\"'"},
	"ruby": {keywords: words("alias and begin break case class def defined? do else elsif end ensure false for if in module next nil not or redo rescue retry return self super then true undef unless until when while yield require"),
		line: []string{"#"}, quotes: "\"'"},
	"sql": {keywords: words("select from where insert into values update set delete create table drop alter add index join left right inner outer on group by order having limit offset as and or not null is in like between distinct union all primary key references default SELECT FROM WHERE INSERT INTO VALUES UPDATE SET DELETE CREATE TABLE DROP ALTER ADD INDEX JOIN LEFT RIGHT INNER OUTER ON GROUP BY ORDER HAVING LIMIT OFFSET AS AND OR NOT NULL IS IN LIKE BETWEEN DISTINCT UNION ALL PRIMARY KEY REFERENCES DEFAULT"),
		line: []string{"--"}, blockOpen: "/*", blockClose: "*/", quotes: "'\""},
	"yaml": {keywords: words("true false null yes no on off"), line: []string{"#"}, quotes: "\"'"},
	"json": {keywords: words("true false null"), quotes: "\""},
	"css":  {keywords: words("important"), blockOpen: "/*", blockClose: "*/", quotes: "\"'"},
	"html": {blockOpen: "<!--", blockClose: "-->", quotes: "\"'"},
}

var langAliases = map[string]string{
	"golang": "go", "py": "python", "python3": "python", "js": "javascript", "jsx": "javascript",
	"ts": "javascript", "tsx": "javascript", "typescript": "javascript", "mjs": "javascript",
	"rs": "rust", "kotlin": "java", "kt": "java", "scala": "java", "cs": "java", "csharp": "java",
	"cpp": "c", "c++": "c", "cc": "c", "h": "c", "hpp": "c", "objc": "c", "swift": "c",
	"sh": "shell", "bash": "shell", "zsh": "shell", "console": "shell", "fish": "shell", "dockerfile": "shell",
	"rb": "ruby", "yml": "yaml", "toml": "yaml", "ini": "yaml", "xml": "html", "svg": "html",
	"scss": "css", "less": "css", "psql": "sql", "postgres": "sql", "mysql": "sql",
}

func specFor(lang string) *langSpec {
	lang = strings.ToLower(lang)
	if i := strings.IndexAny(lang, " {,"); i >= 0 {
		lang = lang[:i]
	}
	if a, ok := langAliases[lang]; ok {
		lang = a
	}
	return langs[lang]
}

// highlight colours one line of code in lang: keywords, types, strings,
// numbers and comments. An unknown language is shown plain, which is the
// honest choice: guessed colours mislead more than none.
func highlight(s Style, lang, line string, st *hlState) string {
	if !s.enabled {
		return line
	}
	if l := strings.ToLower(lang); l == "diff" || l == "patch" {
		switch {
		case strings.HasPrefix(line, "+"):
			return s.Green(line)
		case strings.HasPrefix(line, "-"):
			return s.Red(line)
		case strings.HasPrefix(line, "@@"):
			return s.Cyan(line)
		}
		return line
	}
	sp := specFor(lang)
	if sp == nil {
		return line
	}
	var b strings.Builder
	rs := []rune(line)
	i := 0
	emitWhile := func(style func(string) string, end int) {
		b.WriteString(style(string(rs[i:end])))
		i = end
	}
	for i < len(rs) {
		rest := string(rs[i:])
		switch {
		case st.inComment:
			end := strings.Index(rest, sp.blockClose)
			if end < 0 {
				emitWhile(s.Dim, len(rs))
				continue
			}
			st.inComment = false
			emitWhile(s.Dim, i+len([]rune(rest[:end+len(sp.blockClose)])))
		case st.inString != "":
			end := strings.Index(rest, st.inString)
			if end < 0 {
				emitWhile(s.Green, len(rs))
				continue
			}
			n := len([]rune(rest[:end+len(st.inString)]))
			st.inString = ""
			emitWhile(s.Green, i+n)
		case sp.blockOpen != "" && strings.HasPrefix(rest, sp.blockOpen):
			st.inComment = true
			b.WriteString(s.Dim(sp.blockOpen))
			i += len([]rune(sp.blockOpen))
		case hasLineComment(sp, rest, rs, i):
			emitWhile(s.Dim, len(rs))
		case sp.triple && (strings.HasPrefix(rest, `"""`) || strings.HasPrefix(rest, "'''")):
			st.inString = rest[:3]
			b.WriteString(s.Green(rest[:3]))
			i += 3
		case strings.ContainsRune(sp.quotes, rs[i]):
			q := rs[i]
			j := i + 1
			for j < len(rs) && rs[j] != q {
				if rs[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(rs) {
				if q == '`' {
					st.inString = "`" // a raw string runs on
				}
				emitWhile(s.Green, len(rs))
				continue
			}
			emitWhile(s.Green, j+1)
		case unicode.IsDigit(rs[i]) && (i == 0 || !isIdent(rs[i-1])):
			j := i
			for j < len(rs) && (isIdent(rs[j]) || rs[j] == '.') {
				j++
			}
			emitWhile(s.Yellow, j)
		case isIdent(rs[i]):
			j := i
			for j < len(rs) && isIdent(rs[j]) {
				j++
			}
			w := string(rs[i:j])
			switch {
			case sp.keywords[w]:
				emitWhile(s.Magenta, j)
			case sp.types[w]:
				emitWhile(s.Cyan, j)
			case j < len(rs) && rs[j] == '(':
				emitWhile(s.Blue, j)
			default:
				emitWhile(func(x string) string { return x }, j)
			}
		default:
			b.WriteRune(rs[i])
			i++
		}
	}
	return b.String()
}

func hasLineComment(sp *langSpec, rest string, rs []rune, i int) bool {
	for _, m := range sp.line {
		if strings.HasPrefix(rest, m) {
			// A shell's # inside a word ("a#b", "$#") is not a comment.
			if m == "#" && i > 0 && !unicode.IsSpace(rs[i-1]) {
				return false
			}
			return true
		}
	}
	return false
}

func isIdent(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }
