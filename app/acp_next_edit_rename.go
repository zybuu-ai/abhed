package app

import (
	"cmp"
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
// definition, and offered as one edit; no model is asked and nothing is sent.

// renameHint is Studio's reading of the last edit as a rename: From became
// To, which now starts at At.
type renameHint struct {
	From string  `json:"from"`
	To   string  `json:"to"`
	At   editPos `json:"at"`
}

// keywords are each language's reserved words, which a rename may not produce.
var keywords = map[string]string{
	"python":     "False None True and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield",
	"go":         "break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var",
	"javascript": "await break case catch class const continue debugger default delete do else enum export extends false finally for function if import in instanceof let new null return super switch this throw true try typeof var void while with yield",
	"typescript": "await break case catch class const continue debugger default delete do else enum export extends false finally for function if implements import in instanceof interface let new null package private protected public return static super switch this throw true try typeof var void while with yield",
	"rust":       "as async await break const continue crate dyn else enum extern false fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait true type unsafe use where while",
	"java":       "abstract assert boolean break byte case catch char class const continue default do double else enum extends final finally float for goto if implements import instanceof int interface long native new package private protected public return short static strictfp super switch synchronized this throw throws transient try void volatile while",
}

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

// renameOf is the rename the request describes, Studio's hint first, else
// its last recent edit in this document read as one: an identifier replaced
// by another, the new one in the window where the edit says. line is the
// rename's line within the window.
func renameOf(req nextEditRequest, lines []string) (h renameHint, line int, ok bool) {
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
// lines), and the C family's //, /* */ and quotes.
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
			case py && (strings.HasPrefix(rest, `"""`) || strings.HasPrefix(rest, `'''`)):
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

// scopeOf is the window's lines a rename at line can reach, [from, to): the
// definition it is in, from its top-level header to the next line back at
// that indentation; a rename on a header line that opens a block reaches
// that block. Where the window shows no header above, it is the window.
func scopeOf(lines []string, line int) (int, int) {
	indent := func(s string) int { return len(s) - len(strings.TrimLeft(s, " \t")) }
	blank := func(s string) bool { return strings.TrimSpace(s) == "" }
	header := -1
	t := strings.TrimRight(lines[line], " \t\r\n")
	if indent(lines[line]) == 0 && (strings.HasSuffix(t, ":") || strings.HasSuffix(t, "{")) {
		header = line
	} else if indent(lines[line]) > 0 {
		for i := line - 1; i >= 0; i-- {
			if !blank(lines[i]) && indent(lines[i]) == 0 {
				header = i
				break
			}
		}
	}
	if header < 0 {
		return 0, len(lines)
	}
	end := len(lines)
	for i := header + 1; i < len(lines); i++ {
		if !blank(lines[i]) && indent(lines[i]) == 0 {
			// A closing brace at the header's indentation still belongs to it.
			if strings.HasPrefix(strings.TrimSpace(lines[i]), "}") {
				end = i + 1
			} else {
				end = i
			}
			break
		}
	}
	return header, end
}

// renameEdit is the one edit that finishes a rename in the window: every
// other use of h.From, at the sites the language server named when it named
// any here, else as found by a scan of the rename's scope. Lines are the
// window as Studio sent it (none of it goes anywhere). why says why there is
// none.
func renameEdit(req nextEditRequest, lines []string, h renameHint, line int) ([]suggestedEdit, string) {
	type site struct{ line, at int } // window line, byte offset
	var sites []site
	code := codeOnly(lines, req.Language)
	if len(req.Sites) > 0 {
		for _, r := range req.Sites {
			l := r.Start.Line - req.Window.StartLine
			if r.Start.Line != r.End.Line || l < 0 || l >= len(lines) {
				continue
			}
			at := byteAt(lines[l], r.Start.Character)
			if at >= 0 && byteAt(lines[l], r.End.Character) == at+len(h.From) && wordAt(lines[l], at, h.From) {
				sites = append(sites, site{l, at})
			}
		}
	} else {
		from, to := scopeOf(lines, line)
		for l := from; l < to; l++ {
			for i := 0; i <= len(code[l])-len(h.From); {
				j := strings.Index(code[l][i:], h.From)
				if j < 0 {
					break
				}
				if wordAt(code[l], i+j, h.From) {
					sites = append(sites, site{l, i + j})
				}
				i += j + len(h.From)
			}
		}
	}
	slices.SortFunc(sites, func(a, b site) int { return cmp.Or(a.line-b.line, a.at-b.at) })
	sites = slices.Compact(sites)
	if len(sites) == 0 {
		return nil, "no other use of " + h.From + " was found near the rename"
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
		changed[s.line-first] = l[:s.at] + h.To + l[s.at+len(h.From):]
	}
	end := editPos{Line: req.Window.StartLine + last + 1}
	if !strings.HasSuffix(lines[last], "\n") {
		end = editPos{Line: req.Window.StartLine + last, Character: u16Len(lines[last])}
	}
	return []suggestedEdit{{
		URI:     req.URI,
		Range:   editRange{Start: editPos{Line: req.Window.StartLine + first}, End: end},
		NewText: strings.Join(changed, ""),
	}}, ""
}
