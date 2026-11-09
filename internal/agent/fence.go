package agent

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode"

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

// fenceLookalike is a folder memory tag as text written to imitate one is
// read once folded: any case, spaces, and _ or a space for the hyphen.
var fenceLookalike = regexp.MustCompile(`<[\s]*/?[\s]*folder[\s_-]*memory`)

// foldRune is r as a lookalike is matched: NFKC, lower case, hyphen-like
// dashes as '-', and zero-width characters gone.
func foldRune(r rune) string {
	switch {
	case r == '\u00ad', r == '\u180e', r >= '\u200b' && r <= '\u200f', r >= '\u2060' && r <= '\u2064', r == '\ufeff':
		return ""
	}
	var b strings.Builder
	for _, n := range norm.NFKC.String(string(r)) {
		switch {
		case n >= '\u2010' && n <= '\u2015', n == '\u2212', n == '\ufe58', n == '\ufe63', n == '\uff0d':
			n = '-'
		}
		b.WriteRune(unicode.ToLower(n))
	}
	return b.String()
}

// escapeFolderFence puts a backslash before every folder memory tag in tool
// output, and every lookalike of one (another case, a space after the <, a
// zero-width or full-width character, another dash), so none can pass for
// the fence Abhed puts around a folder's memory.
func escapeFolderFence(s string) string {
	if !strings.ContainsAny(s, "<\uff1c\ufe64") {
		return s
	}
	var folded strings.Builder
	var at []int // for each byte of folded, the offset in s of the rune it came from
	for i, r := range s {
		f := foldRune(r)
		folded.WriteString(f)
		for range len(f) {
			at = append(at, i)
		}
	}
	locs := fenceLookalike.FindAllStringIndex(folded.String(), -1)
	if len(locs) == 0 {
		return s
	}
	var out strings.Builder
	prev := 0
	for _, loc := range locs {
		start := at[loc[0]]
		out.WriteString(s[prev:start])
		out.WriteByte('\\')
		prev = start
	}
	out.WriteString(s[prev:])
	return out.String()
}
