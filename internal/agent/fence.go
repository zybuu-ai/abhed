package agent

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// FolderMemoryTag begins the tag a folder's memory is fenced with. The rest
// of the tag is a random fence for the session, named only in the system
// prompt, so no file or command can know it.
const FolderMemoryTag = "folder-memory-"

// newFence is a session's random fence.
func newFence() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// A folder memory tag, as read once folded, is a < followed by a short run of
// anything that is not a letter or a digit, then "folder", then up to
// maxTagGap such characters, then "memory". Code is told apart only where
// the words touch, as in an identifier: a generic type `List<FolderMemory>`
// (a word right before the <) and Go's `<-folderMemory` (punctuation right
// after it) are left alone, while a tag with anything between the words is
// escaped whatever comes before the <. An HTML comment's `<!--` is not a tag.
const maxTagGap = 64

// openers are the characters that fold to the tag's opening <.
const openers = "<＜﹤‹〈⟨〈"

// homoglyphs are letters drawn like the Latin ones of "folder memory" and
// their neighbours: Cyrillic, Greek, and the digits and capitals read as letters.
var homoglyphs = map[rune]rune{
	'а': 'a', 'е': 'e', 'ё': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'у': 'y',
	'х': 'x', 'к': 'k', 'м': 'm', 'т': 't', 'ԁ': 'd', 'ӏ': 'l', 'г': 'r',
	'і': 'i', 'ј': 'j', 'ѕ': 's', 'ɡ': 'g',
	'ο': 'o', 'ε': 'e', 'ρ': 'p', 'μ': 'm', 'υ': 'u', 'γ': 'y', 'ν': 'v',
	'κ': 'k', 'τ': 't', 'ι': 'i', 'α': 'a', 'σ': 'o', 'δ': 'd',
	'0': 'o', '1': 'l', '|': 'l', 'I': 'l',
	'\u2039': '<', '\u3008': '<', '\u27e8': '<', '\u2329': '<',
}

// asciiFold is foldRune for each ASCII byte.
var asciiFold = func() (t [utf8.RuneSelf]byte) {
	for c := range t {
		t[c] = byte(foldRune(nil, rune(c))[0])
	}
	return t
}()

// foldRune appends r as a lookalike is matched: decomposed, combining marks
// and format characters (zero-width ones) dropped, homoglyphs read as the
// Latin letter, and lower case.
func foldRune(dst []rune, r rune) []rune {
	if r < utf8.RuneSelf {
		if h, ok := homoglyphs[r]; ok {
			return append(dst, h)
		}
		return append(dst, unicode.ToLower(r))
	}
	if unicode.Is(unicode.Cf, r) {
		return dst
	}
	for _, n := range norm.NFKD.String(string(r)) {
		if unicode.Is(unicode.Mn, n) || unicode.Is(unicode.Me, n) || unicode.Is(unicode.Cf, n) {
			continue
		}
		if h, ok := homoglyphs[n]; ok {
			n = h
		} else if h, ok := homoglyphs[unicode.ToLower(n)]; ok {
			n = h
		}
		dst = append(dst, unicode.ToLower(n))
	}
	return dst
}

// mayHoldFence is the cheap test most output passes: a lookalike needs an
// opening character, and in plain ASCII the letters of "emory" once folded.
// ascii reports whether s is all ASCII, which folds byte for byte.
func mayHoldFence(s string) (may, ascii bool) {
	ascii = true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.IndexByte(s, '<') >= 0 && containsFoldASCII(s, "emory"), true
	}
	return strings.ContainsAny(s, openers), false
}

// containsFoldASCII reports whether s holds sub once folded as a lookalike
// is (case, and digits read as letters); sub is folded already.
func containsFoldASCII(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		k := 0
		for k < len(sub) && asciiFold[s[i+k]] == sub[k] {
			k++
		}
		if k == len(sub) {
			return true
		}
	}
	return false
}

// escapeFolderFence puts a backslash before every folder memory tag in tool
// output, and every lookalike of one (another case, spaces or other
// separators, zero-width, full-width or combining characters, homoglyphs,
// other dashes or angle brackets), so none can pass for the fence Abhed
// puts around a folder's memory. A tag after an odd number of backslashes is
// escaped already and left as it is, so text relayed from one loop to
// another is not escaped twice.
func escapeFolderFence(s string) string {
	may, ascii := mayHoldFence(s)
	if !may {
		return s
	}
	var text []byte
	var from []int32 // for each byte of text, where in s it came from; nil when byte for byte
	if ascii {
		text = []byte(s)
		for i, c := range text {
			text[i] = asciiFold[c]
		}
	} else {
		text = make([]byte, 0, len(s))
		from = make([]int32, 0, len(s))
		var buf []rune
		for i, r := range s {
			if r < utf8.RuneSelf {
				text, from = append(text, asciiFold[r]), append(from, int32(i)) // #nosec G115 -- tool output is far below 2 GiB
				continue
			}
			buf = foldRune(buf[:0], r)
			for _, f := range buf {
				n := len(text)
				text = utf8.AppendRune(text, f)
				for range len(text) - n {
					from = append(from, int32(i)) // #nosec G115 -- as above
				}
			}
		}
	}
	locs := lookalikes(text)
	if len(locs) == 0 {
		return s
	}
	var out strings.Builder
	prev := 0
	for _, at := range locs {
		start := at
		if from != nil {
			start = int(from[at])
		}
		if start < prev || escapedAt(s, start) {
			continue
		}
		out.WriteString(s[prev:start])
		out.WriteByte('\\')
		prev = start
	}
	out.WriteString(s[prev:])
	return out.String()
}

// escapedAt reports whether an odd number of backslashes comes right before
// s[i]: then the character there is escaped, where an even number is
// backslashes escaping each other.
func escapedAt(s string, i int) bool {
	n := 0
	for i-n > 0 && s[i-n-1] == '\\' {
		n++
	}
	return n%2 == 1
}

// lookalikes are where each lookalike tag starts in folded text, in order:
// from each "folder", back over other characters to a <, and on over
// separators to "memory". Linear, with no expression, so a large output with
// many tags or near-tags costs little more than one without.
func lookalikes(text []byte) []int {
	var out []int
	word := []byte("folder")
	for pos := 0; ; {
		i := bytes.Index(text[pos:], word)
		if i < 0 {
			return out
		}
		f := pos + i
		pos = f + len(word)
		start := openerBefore(text, f)
		if start < 0 || bytes.HasPrefix(text[start+1:], []byte("!--")) {
			continue
		}
		gap, ok := memoryAfter(text, pos)
		if !ok {
			continue
		}
		if gap == 0 && (wordByte(text, start-1) || punctAfter(text, start)) {
			continue // an identifier in code: List<FolderMemory>, <-folderMemory
		}
		if len(out) == 0 || out[len(out)-1] < start {
			out = append(out, start)
		}
	}
}

// openerBefore is where the < before text[f] is, past characters that are
// not letters or digits, or -1.
func openerBefore(text []byte, f int) int {
	j := f
	for n := 0; j > 0 && n <= maxTagGap; n++ {
		r, size := utf8.DecodeLastRune(text[:j])
		if r == '<' {
			return j - size
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return -1
		}
		j -= size
	}
	return -1
}

// memoryAfter reports whether "memory" follows text[k], past at most
// maxTagGap characters that are not letters or digits, and how many.
func memoryAfter(text []byte, k int) (gap int, ok bool) {
	for n := 0; k < len(text) && n <= maxTagGap; n++ {
		r, size := utf8.DecodeRune(text[k:])
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return n, bytes.HasPrefix(text[k:], []byte("memory"))
		}
		k += size
	}
	return 0, false
}

// punctAfter reports whether the < at text[i] is followed by punctuation,
// not a space, a slash or a letter: an operator such as <- or <=.
func punctAfter(text []byte, i int) bool {
	if i+1 >= len(text) {
		return false
	}
	r, _ := utf8.DecodeRune(text[i+1:])
	return r != '/' && !unicode.IsSpace(r) && !unicode.Is(unicode.Z, r) && !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// wordByte reports whether text[i] is a letter, a digit or _ (false before the start).
func wordByte(text []byte, i int) bool {
	if i < 0 {
		return false
	}
	c := text[i]
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
}
