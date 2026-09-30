package ui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/width"
)

// runeWidth is the number of terminal columns r occupies on its own.
//
// Counting runes put the cursor in the middle of the text after the first
// wide character: a CJK character or most emoji take two columns, and a
// combining accent takes none.
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || r == 0x7f:
		return 0
	case r < 0x300:
		return 1 // Latin and most punctuation: the common case, answered fast
	case joins(r):
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	// Emoji pictographs are wide in every modern terminal, though older
	// Unicode tables list some of them as neutral.
	if r >= 0x1f300 && r <= 0x1faff || r >= 0x1f000 && r <= 0x1f02f || r >= 0x1f0a0 && r <= 0x1f0ff {
		return 2
	}
	return 1
}

// joins reports whether r attaches to the character before it rather than
// starting one of its own: combining marks, the zero-width joiner, variation
// selectors, skin-tone modifiers and tag characters.
func joins(r rune) bool {
	switch {
	case r == 0x200d, r == 0x200c:
		return true
	case r >= 0xfe00 && r <= 0xfe0f, r >= 0xe0100 && r <= 0xe01ef:
		return true
	case r >= 0x1f3fb && r <= 0x1f3ff:
		return true
	case r >= 0xe0020 && r <= 0xe007f:
		return true
	case r == 0x20e3: // combining enclosing keycap
		return true
	}
	return r >= 0x300 && (unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Mc, r))
}

func regional(r rune) bool { return r >= 0x1f1e6 && r <= 0x1f1ff }

// clusterEnd returns the index in rs just past the grapheme cluster that
// starts at i: what a person sees as one character, and what the cursor
// steps over and Backspace removes whole.
//
// It covers what terminals actually draw as one cell or two: a base with its
// combining marks, emoji joined by ZWJ with their modifiers and selectors,
// flag pairs, and CR LF. It is not the full UAX #29, which terminals do not
// follow either.
func clusterEnd(rs []rune, i int) int {
	if i >= len(rs) {
		return len(rs)
	}
	j := i + 1
	if rs[i] == '\r' && j < len(rs) && rs[j] == '\n' {
		return j + 1
	}
	if regional(rs[i]) && j < len(rs) && regional(rs[j]) {
		j++
	}
	for j < len(rs) {
		switch {
		case joins(rs[j]):
			if rs[j] == 0x200d && j+1 < len(rs) {
				j += 2 // the joiner and what it joins
				continue
			}
			j++
		default:
			return j
		}
	}
	return j
}

// clusterStart returns the start of the cluster that ends at i (exclusive).
func clusterStart(rs []rune, i int) int {
	if i <= 0 {
		return 0
	}
	// Back up to a rune that certainly begins a cluster, then walk forward:
	// clusters are short, so the walk is too.
	start := i - 1
	for start > 0 && (joins(rs[start]) || rs[start-1] == 0x200d) {
		start--
	}
	for start > 0 && regional(rs[start]) && regional(rs[start-1]) {
		start-- // flags pair from the start of their run
	}
	for p := start; ; {
		e := clusterEnd(rs, p)
		if e >= i {
			return p
		}
		p = e
	}
}

// clusterWidth is the columns a cluster takes. The base decides, except that
// an emoji-presentation selector or a keycap makes a narrow base wide, and a
// flag pair is wide.
func clusterWidth(c []rune) int {
	if len(c) == 0 {
		return 0
	}
	if c[0] == '\t' {
		return 4
	}
	w := runeWidth(c[0])
	if len(c) > 1 {
		if regional(c[0]) {
			return 2
		}
		for _, r := range c[1:] {
			if r == 0xfe0f || r == 0x20e3 {
				return 2
			}
		}
	}
	return w
}

// runesWidth is the display width of rs, cluster by cluster.
func runesWidth(rs []rune) int {
	n := 0
	for i := 0; i < len(rs); {
		e := clusterEnd(rs, i)
		n += clusterWidth(rs[i:e])
		i = e
	}
	return n
}

// displayWidth is the column width of s, skipping ANSI escape sequences so a
// coloured string measures what it shows.
func displayWidth(s string) int {
	n := 0
	forEachToken(s, func(tok string, esc bool) {
		if !esc {
			n += clusterWidth([]rune(tok))
		}
	})
	return n
}

// forEachToken splits s into escape sequences and grapheme clusters, in order.
// Rendering and diffing both need a string cut only between whole tokens: a
// cut inside an escape leaves the terminal in a mode nobody asked for, and a
// cut inside a cluster draws half a character.
func forEachToken(s string, f func(tok string, esc bool)) {
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			j := escEnd(s, i)
			f(s[i:j], true)
			i = j
			continue
		}
		// A run of ordinary text up to the next escape, split into clusters.
		j := strings.IndexByte(s[i:], 0x1b)
		if j < 0 {
			j = len(s)
		} else {
			j += i
		}
		seg := s[i:j]
		if isASCII(seg) {
			for k := 0; k < len(seg); k++ {
				f(seg[k:k+1], false)
			}
		} else {
			rs := []rune(seg)
			for k := 0; k < len(rs); {
				e := clusterEnd(rs, k)
				f(string(rs[k:e]), false)
				k = e
			}
		}
		i = j
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// escEnd returns the index just past the escape sequence starting at i.
// CSI ends at a final byte in @..~; OSC ends at BEL or ST; anything else is
// the escape and one character.
func escEnd(s string, i int) int {
	if i+1 >= len(s) {
		return len(s)
	}
	switch s[i+1] {
	case '[':
		for j := i + 2; j < len(s); j++ {
			if s[j] >= 0x40 && s[j] <= 0x7e {
				return j + 1
			}
		}
		return len(s)
	case ']', 'P', '_', '^':
		for j := i + 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
		}
		return len(s)
	}
	return i + 2
}

// truncateWidth cuts s to at most w columns, marking the cut with an
// ellipsis. It never splits a character or an escape, and keeps the escapes
// after the cut so styling is closed where it was opened.
func truncateWidth(s string, w int) string {
	if displayWidth(s) <= w {
		return s
	}
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	used, cut := 0, false
	forEachToken(s, func(tok string, esc bool) {
		if esc {
			b.WriteString(tok)
			return
		}
		if cut {
			return
		}
		cw := clusterWidth([]rune(tok))
		if used+cw > w-1 {
			b.WriteString("…")
			cut = true
			return
		}
		b.WriteString(tok)
		used += cw
	})
	return b.String()
}

// padRight pads s with spaces to w columns.
func padRight(s string, w int) string {
	if n := w - displayWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// stripANSI removes escape sequences.
func stripANSI(s string) string {
	if strings.IndexByte(s, 0x1b) < 0 {
		return s
	}
	var b strings.Builder
	forEachToken(s, func(tok string, esc bool) {
		if !esc {
			b.WriteString(tok)
		}
	})
	return b.String()
}

// hardWrap splits s into rows of at most w columns, cutting between clusters
// and carrying the SGR state across the cut so each row stands alone.
func hardWrap(s string, w int) []string {
	if w < 1 {
		w = 1
	}
	if displayWidth(s) <= w {
		return []string{s}
	}
	var rows []string
	var b strings.Builder
	var sgr []string // styling in force, replayed at the start of each row
	used := 0
	forEachToken(s, func(tok string, esc bool) {
		if esc {
			b.WriteString(tok)
			if isSGR(tok) {
				if isReset(tok) {
					sgr = sgr[:0]
				} else {
					sgr = append(sgr, tok)
				}
			}
			return
		}
		cw := clusterWidth([]rune(tok))
		if used+cw > w && used > 0 {
			if len(sgr) > 0 {
				b.WriteString("\x1b[0m")
			}
			rows = append(rows, b.String())
			b.Reset()
			for _, e := range sgr {
				b.WriteString(e)
			}
			used = 0
		}
		b.WriteString(tok)
		used += cw
	})
	rows = append(rows, b.String())
	return rows
}

func isSGR(tok string) bool { return len(tok) >= 3 && tok[1] == '[' && tok[len(tok)-1] == 'm' }

func isReset(tok string) bool { return tok == "\x1b[0m" || tok == "\x1b[m" }

// sanitize removes control characters that would move the cursor or change
// the terminal's mode, keeping SGR styling when keepSGR is set. Text from a
// model, a tool or a status command is drawn through it, so it cannot draw
// over the dock or retitle the window.
func sanitize(s string, keepSGR bool) string {
	var b strings.Builder
	forEachToken(s, func(tok string, esc bool) {
		if esc {
			if keepSGR && isSGR(tok) && safeSGR(tok) {
				b.WriteString(tok)
			}
			return
		}
		r, _ := utf8.DecodeRuneInString(tok)
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r == '\n':
			b.WriteByte('\n')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
		case r == 0x202e || r == 0x202d || r == 0x2066 || r == 0x2067 || r == 0x2068 || r == 0x2069 || r == 0x202a || r == 0x202b || r == 0x202c:
			// Bidi overrides reorder what is shown against what is there.
		default:
			b.WriteString(tok)
		}
	})
	return b.String()
}

// safeSGR accepts only parameters that are digits and separators.
func safeSGR(tok string) bool {
	for _, c := range tok[2 : len(tok)-1] {
		if (c < '0' || c > '9') && c != ';' && c != ':' {
			return false
		}
	}
	return true
}
