package ui

import (
	"strings"
)

// Markdown renders a model's reply for a terminal, without wrapping: for
// output that is not a terminal the dock can measure.
//
// Models answer in markdown whether or not anything asked them to, so a raw
// print shows the reader `**z/OS**` and a table drawn in pipes. This is a
// small renderer rather than a dependency: it handles what a model actually
// emits — headings, emphasis, inline code, fenced code, lists, tables, rules,
// quotes, links — and leaves what it does not recognise as written, which is
// the safe direction: an unrendered line is readable, a mangled one is not.
func Markdown(s Style, text string) string {
	return strings.Join(renderMarkdown(s, text, 0), "\n")
}

// renderMarkdown renders text as rows of at most width columns; 0 does not
// wrap.
func renderMarkdown(s Style, text string, width int) []string {
	var st mdState
	var out []string
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		out = append(out, st.line(s, line, width)...)
	}
	return append(out, st.flush(s, width)...)
}

// mdState carries what a line's rendering depends on from the lines before
// it: an open code fence, and table rows held until the table ends.
type mdState struct {
	fence     string // the opening marker while in a fenced block
	lang      string
	hl        hlState
	table     []string
	lastBlank bool
	started   bool
}

// line renders one complete source line. A table row is held, since a table
// can only be laid out once its widest row is known.
func (st *mdState) line(s Style, src string, width int) []string {
	src = strings.TrimRight(src, "\r")
	if st.fence != "" {
		if t := strings.TrimSpace(src); strings.HasPrefix(t, st.fence) && strings.Trim(t, st.fence[:1]) == "" {
			st.fence, st.lang = "", ""
			return nil
		}
		return st.code(s, src, width)
	}
	var out []string
	if isTableRow(src) {
		st.table = append(st.table, src)
		return nil
	}
	out = append(out, st.flush(s, width)...)

	t := strings.TrimSpace(src)
	if marker := fenceMarker(t); marker != "" {
		st.fence = marker
		st.lang = strings.ToLower(strings.TrimSpace(strings.TrimLeft(t, marker[:1])))
		st.hl = hlState{}
		st.lastBlank = false
		st.started = true
		return out
	}
	if t == "" {
		if st.lastBlank || !st.started {
			return out
		}
		st.lastBlank = true
		return append(out, "")
	}
	st.lastBlank = false
	st.started = true
	return append(out, renderBlockLine(s, src, width, false)...)
}

// flush renders held table rows: as a table when they are one, otherwise as
// the lines they were.
func (st *mdState) flush(s Style, width int) []string {
	rows := st.table
	st.table = nil
	if len(rows) == 0 {
		return nil
	}
	st.started, st.lastBlank = true, false
	if len(rows) >= 2 && isTableDivider(rows[1]) {
		cells := [][]string{splitRow(rows[0])}
		for _, r := range rows[2:] {
			cells = append(cells, splitRow(r))
		}
		return renderTable(s, cells, width)
	}
	var out []string
	for _, r := range rows {
		out = append(out, renderBlockLine(s, r, width, false)...)
	}
	return out
}

// peek renders a line that is still arriving, without changing the state:
// what a streamed reply shows before its newline. Emphasis that has opened
// and not yet closed is shown as emphasis, so the markers never flash up.
func (st *mdState) peek(s Style, src string, width int) []string {
	if src == "" {
		return nil
	}
	if st.fence != "" {
		t := strings.TrimSpace(src)
		if t != "" && strings.Trim(t, st.fence[:1]) == "" {
			return nil // what may be the closing fence
		}
		cp := st.hl
		rows := st.code(s, src, width)
		st.hl = cp
		return rows
	}
	t := strings.TrimSpace(src)
	if strings.HasPrefix(t, "`") || strings.HasPrefix(t, "~~~") || strings.HasPrefix(t, "|") {
		// A fence or a table, not yet known: shown as typed until its line ends.
		return []string{s.Dim(truncateWidth(src, max(width, 1)))}
	}
	return renderBlockLine(s, src, width, true)
}

func fenceMarker(t string) string {
	for _, m := range []string{"```", "~~~"} {
		if strings.HasPrefix(t, m) {
			n := len(t) - len(strings.TrimLeft(t, m[:1]))
			return strings.Repeat(m[:1], n)
		}
	}
	return ""
}

// code renders a line inside a fenced block: highlighted, indented, and cut
// rather than word-wrapped, since spaces in code are not places to break.
func (st *mdState) code(s Style, src string, width int) []string {
	src = strings.ReplaceAll(src, "\t", "    ")
	lit := highlight(s, st.lang, src, &st.hl)
	if width <= 0 {
		return []string{"  " + lit}
	}
	rows := hardWrap(lit, max(width-4, 8))
	for i := range rows {
		lead := "  "
		if i > 0 {
			lead = "  " + s.Dim("↪ ")
		}
		rows[i] = lead + rows[i]
	}
	return rows
}

// renderBlockLine renders a line outside code: a heading, rule, quote, list
// item or paragraph line.
func renderBlockLine(s Style, line string, width int, lenient bool) []string {
	trimmed := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(trimmed)]
	t := strings.TrimSpace(line)

	if t == "---" || t == "***" || t == "___" {
		n := 40
		if width > 0 {
			n = min(width, 60)
		}
		return []string{s.Dim(strings.Repeat("─", n))}
	}

	// Headings are shown by weight, never by hashes or by changing case.
	if h := strings.TrimLeft(trimmed, "#"); len(h) < len(trimmed) && len(trimmed)-len(h) <= 6 && strings.HasPrefix(h, " ") {
		level := len(trimmed) - len(h)
		segs := parseInline(strings.TrimSpace(h), lenient)
		for i := range segs {
			segs[i].bold = true
			if level == 1 {
				segs[i].accent = true
			}
		}
		return wrapSegs(s, segs, width, "", "")
	}

	for _, marker := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(trimmed, marker) {
			lead := indent + s.Dim("•") + " "
			return wrapSegs(s, parseInline(strings.TrimLeft(trimmed[2:], " "), lenient), width, lead, indent+"  ")
		}
	}
	if n, rest, ok := numberedItem(trimmed); ok {
		lead := indent + s.Dim(n+".") + " "
		return wrapSegs(s, parseInline(rest, lenient), width, lead, indent+strings.Repeat(" ", len(n)+2))
	}
	if strings.HasPrefix(trimmed, ">") {
		body := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
		segs := parseInline(body, lenient)
		for i := range segs {
			segs[i].dim = true
		}
		return wrapSegs(s, segs, width, indent+s.Dim("│ "), indent+s.Dim("│ "))
	}
	return wrapSegs(s, parseInline(trimmed, lenient), width, indent, indent)
}

// seg is a run of text with one style.
type seg struct {
	text                            string
	bold, italic, code, strike, dim bool
	accent                          bool
	link                            bool
}

func (g seg) render(s Style) string {
	t := g.text
	switch {
	case g.code:
		t = s.Code(t)
	case g.link:
		t = s.Dim(t)
	}
	if g.bold {
		t = s.Bold(t)
	}
	if g.italic {
		t = s.Italic(t)
	}
	if g.strike {
		t = s.Strike(t)
	}
	if g.accent {
		t = s.Accent(t)
	}
	if g.dim && !g.code {
		t = s.Dim(t)
	}
	return t
}

func (g seg) sameStyle(o seg) bool {
	a, b := g, o
	a.text, b.text = "", ""
	return a == b
}

// parseInline splits text into styled runs: `code`, **bold**, *italic* or
// _italic_, ~~strike~~ and [links](url). A marker that never closes is left
// as written — except, when lenient, ** and ` in text still arriving, which
// are taken as opened.
func parseInline(text string, lenient bool) []seg {
	var out []seg
	var cur seg
	var b strings.Builder
	emit := func() {
		if b.Len() > 0 {
			c := cur
			c.text = b.String()
			out = append(out, c)
			b.Reset()
		}
	}
	for i := 0; i < len(text); {
		rest := text[i:]
		switch {
		case rest[0] == '`':
			n := len(rest) - len(strings.TrimLeft(rest, "`"))
			tick := rest[:n]
			end := strings.Index(rest[n:], tick)
			if end < 0 && !lenient {
				b.WriteString(tick)
				i += n
				continue
			}
			emit()
			code := rest[n:]
			if end >= 0 {
				code = rest[n : n+end]
				i += n + end + n
			} else {
				i = len(text)
			}
			c := cur
			c.code = true
			c.text = code
			if t := strings.TrimSpace(code); t != "" {
				c.text = t
			}
			if c.text != "" {
				out = append(out, c)
			}
			continue
		case strings.HasPrefix(rest, "**") || strings.HasPrefix(rest, "__"):
			m := rest[:2]
			opens := len(rest) > 2 && rest[2] != ' ' && (strings.Contains(rest[2:], m) || lenient)
			if cur.bold || opens {
				emit()
				cur.bold = !cur.bold
				i += 2
				continue
			}
		case strings.HasPrefix(rest, "~~"):
			if cur.strike || strings.Contains(rest[2:], "~~") {
				emit()
				cur.strike = !cur.strike
				i += 2
				continue
			}
		case rest[0] == '*' || rest[0] == '_':
			m := rest[:1]
			if cur.italic {
				emit()
				cur.italic = false
				i++
				continue
			}
			prevWord := i > 0 && isWordByte(text[i-1])
			if len(rest) > 1 && rest[1] != ' ' && !prevWord {
				if j := strings.Index(rest[1:], m); j > 0 && rest[j] != ' ' {
					emit()
					cur.italic = true
					i++
					continue
				}
			}
		case rest[0] == '[':
			if close := strings.Index(rest, "]("); close > 0 {
				if end := strings.IndexByte(rest[close:], ')'); end > 0 {
					label := rest[1:close]
					url := rest[close+2 : close+end]
					emit()
					out = append(out, parseInline(label, false)...)
					l := cur
					l.link = true
					l.text = " (" + url + ")"
					out = append(out, l)
					i += close + end + 1
					continue
				}
			}
		}
		b.WriteByte(text[i])
		i++
	}
	emit()
	return out
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

// wrapSegs lays styled runs into rows of at most width columns, breaking
// only between words; lead begins the first row and hang every other.
func wrapSegs(s Style, segs []seg, width int, lead, hang string) []string {
	type word struct {
		parts []seg
		w     int
		space bool
	}
	var words []word
	for _, g := range segs {
		for _, piece := range splitKeepSpaces(g.text) {
			p := g
			p.text = piece
			sp := strings.TrimSpace(piece) == ""
			if len(words) > 0 && !sp && !words[len(words)-1].space {
				last := &words[len(words)-1]
				last.parts = append(last.parts, p)
				last.w += displayWidth(piece)
				continue
			}
			words = append(words, word{parts: []seg{p}, w: displayWidth(piece), space: sp})
		}
	}
	avail := 0
	if width > 0 {
		avail = max(width-displayWidth(lead), 8)
	}
	var rows []string
	var row []seg
	used := 0
	flush := func() {
		prefix := hang
		if len(rows) == 0 {
			prefix = lead
		}
		for len(row) > 0 && strings.TrimSpace(row[len(row)-1].text) == "" {
			row = row[:len(row)-1]
		}
		rows = append(rows, prefix+renderRuns(s, row))
		row, used = nil, 0
	}
	for _, w := range words {
		switch {
		case avail == 0 || used+w.w <= avail:
			if w.space && used == 0 && len(rows) > 0 {
				continue // no space at the start of a wrapped row
			}
			row = append(row, w.parts...)
			used += w.w
		case w.space:
			flush()
		case w.w > avail:
			// A word wider than a row is the one thing cut.
			for _, p := range w.parts {
				for _, piece := range hardWrap(p.text, avail) {
					pw := displayWidth(piece)
					if used > 0 && used+pw > avail {
						flush()
					}
					q := p
					q.text = piece
					row = append(row, q)
					used += pw
				}
			}
		default:
			flush()
			row = append(row, w.parts...)
			used = w.w
		}
	}
	if len(row) > 0 || len(rows) == 0 {
		flush()
	}
	return rows
}

// renderRuns styles runs, merging neighbours of the same style so a row
// carries one escape per change rather than one per word.
func renderRuns(s Style, runs []seg) string {
	var b strings.Builder
	for i := 0; i < len(runs); {
		j := i + 1
		text := runs[i].text
		for j < len(runs) && runs[j].sameStyle(runs[i]) {
			text += runs[j].text
			j++
		}
		g := runs[i]
		g.text = text
		b.WriteString(g.render(s))
		i = j
	}
	return b.String()
}

func numberedItem(s string) (string, string, bool) {
	for i, c := range s {
		if c >= '0' && c <= '9' {
			continue
		}
		if (c == '.' || c == ')') && i > 0 && i+1 < len(s) && s[i+1] == ' ' {
			return s[:i], s[i+2:], true
		}
		return "", "", false
	}
	return "", "", false
}

func isTableRow(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "|") && strings.HasSuffix(t, "|") && len(t) > 1
}

func isTableDivider(line string) bool {
	if !isTableRow(line) {
		return false
	}
	for _, c := range strings.TrimSpace(line) {
		if c != '|' && c != '-' && c != ':' && c != ' ' {
			return false
		}
	}
	return true
}

func splitRow(line string) []string {
	t := strings.Trim(strings.TrimSpace(line), "|")
	parts := strings.Split(t, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// renderTable aligns the columns, which is the whole reason a table was used.
// A table wider than the screen is shown a record at a time instead, since
// a table cut at the edge loses exactly the columns that made it one.
func renderTable(s Style, rows [][]string, width int) []string {
	cols := 0
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	widths := make([]int, cols)
	cellText := func(c string) string { return renderRuns(s, parseInline(c, false)) }
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], displayWidth(cellText(c)))
		}
	}
	total := 2
	for _, w := range widths {
		total += w + 2
	}
	if width > 0 && total > width {
		var out []string
		head := rows[0]
		for _, r := range rows[1:] {
			for i, c := range r {
				name := ""
				if i < len(head) {
					name = head[i]
				}
				out = append(out, wrapSegs(s, append([]seg{{text: name + ": ", bold: true}}, parseInline(c, false)...), width, "  ", "    ")...)
			}
			out = append(out, "")
		}
		return out
	}
	var out []string
	for n, r := range rows {
		var b strings.Builder
		b.WriteString("  ")
		for i := 0; i < cols; i++ {
			c := ""
			if i < len(r) {
				c = r[i]
			}
			text := cellText(c)
			if n == 0 {
				text = s.Bold(stripANSI(text))
			}
			b.WriteString(text)
			if i < cols-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-displayWidth(text)+2))
			}
		}
		out = append(out, strings.TrimRight(b.String(), " "))
		if n == 0 {
			var rule []string
			for _, w := range widths {
				rule = append(rule, s.Dim(strings.Repeat("─", w)))
			}
			out = append(out, "  "+strings.Join(rule, "  "))
		}
	}
	return out
}

// mdBlock is a finished reply, kept as its source so it can be laid out
// again at any width.
type mdBlock struct {
	src  string
	lead string // the first row's marker
}

func (b *mdBlock) lines(width int, s Style, _ bool) []string {
	return indentRows(renderMarkdown(s, b.src, max(width-2, 10)), b.lead)
}

// indentRows puts a reply's marker before its first row and two spaces
// before the rest, so a reply reads as one unit in a long transcript.
func indentRows(rows []string, lead string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		switch {
		case i == 0 && lead != "":
			out[i] = lead + r
		case r == "":
			out[i] = ""
		default:
			out[i] = "  " + r
		}
	}
	return out
}
