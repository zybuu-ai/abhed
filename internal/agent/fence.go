package agent

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
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

// fenceLookalike is a folder memory tag as read once folded: any case, and
// anything that is not a letter or a digit, a few of them at most, around
// the slash and between the words.
var fenceLookalike = regexp.MustCompile(`<[^a-z0-9]{0,8}/?[^a-z0-9]{0,8}folder[^a-z0-9]{0,8}memory`)

// homoglyphs are letters drawn like the Latin ones of "folder memory" and
// their neighbours: Cyrillic, Greek, and the digits and capitals read as letters.
var homoglyphs = map[rune]rune{
	'а': 'a', 'е': 'e', 'ё': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'у': 'y',
	'х': 'x', 'к': 'k', 'м': 'm', 'т': 't', 'ԁ': 'd', 'ӏ': 'l', 'г': 'r',
	'і': 'i', 'ј': 'j', 'ѕ': 's', 'ɡ': 'g',
	'ο': 'o', 'ε': 'e', 'ρ': 'p', 'μ': 'm', 'υ': 'u', 'γ': 'y', 'ν': 'v',
	'κ': 'k', 'τ': 't', 'ι': 'i', 'α': 'a', 'σ': 'o', 'δ': 'd',
	'0': 'o', '1': 'l', '|': 'l', 'I': 'l',
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
	return strings.ContainsAny(s, "<＜﹤"), false
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
// other dashes), so none can pass for the fence Abhed puts around a
// folder's memory. One already escaped is left as it is, so text relayed
// from one loop to another is not escaped twice.
func escapeFolderFence(s string) string {
	may, ascii := mayHoldFence(s)
	if !may {
		return s
	}
	var text string
	var from []int // where each folded rune came from in s; nil when byte for byte
	if ascii {
		b := []byte(s)
		for i, c := range b {
			b[i] = asciiFold[c]
		}
		text = string(b)
	} else {
		folded := make([]rune, 0, len(s))
		from = make([]int, 0, len(s))
		for i, r := range s {
			if r < utf8.RuneSelf {
				folded, from = append(folded, rune(asciiFold[r])), append(from, i)
				continue
			}
			n := len(folded)
			folded = foldRune(folded, r)
			for range len(folded) - n {
				from = append(from, i)
			}
		}
		text = string(folded)
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
			start = from[utf8.RuneCountInString(text[:at])]
		}
		if start < prev || start > 0 && s[start-1] == '\\' {
			continue // already escaped
		}
		out.WriteString(s[prev:start])
		out.WriteByte('\\')
		prev = start
	}
	out.WriteString(s[prev:])
	return out.String()
}

// lookalikes are where each lookalike tag starts in folded text. The
// expression runs only on the few bytes before each "memory", not the whole
// of a large output.
func lookalikes(text string) []int {
	var out []int
	for pos := 0; ; {
		i := strings.Index(text[pos:], "memory")
		if i < 0 {
			return out
		}
		end := pos + i + len("memory")
		from := max(0, end-128)
		if loc := fenceLookalike.FindStringIndex(text[from:end]); loc != nil && from+loc[1] == end {
			if at := from + loc[0]; len(out) == 0 || out[len(out)-1] != at {
				out = append(out, at)
			}
		}
		pos = end
	}
}
