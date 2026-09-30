package ui

import (
	"strings"
	"sync"
)

// block is one finished piece of the transcript. It renders itself at any
// width, so a resize draws the transcript again instead of leaving the
// terminal's reflow of the old rows, and expanded is the Ctrl-O view: the
// whole of a tool's output or of the reasoning, where the transcript shows a
// preview.
type block interface {
	lines(width int, s Style, expanded bool) []string
}

// transcript keeps the blocks a session has committed, up to a bound.
type transcript struct {
	mu     sync.Mutex
	blocks []block
	max    int
}

func (t *transcript) add(b block) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.blocks = append(t.blocks, b)
	if t.max > 0 && len(t.blocks) > t.max {
		t.blocks = append([]block(nil), t.blocks[len(t.blocks)-t.max:]...)
	}
}

// replace swaps old for new where old is, so a reply streamed as rows is
// kept as its markdown once complete.
func (t *transcript) replace(old, new block) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.blocks) - 1; i >= 0; i-- {
		if t.blocks[i] == old {
			t.blocks[i] = new
			return
		}
	}
}

func (t *transcript) snapshot() []block {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]block(nil), t.blocks...)
}

// tail renders the last blocks until at least want rows, oldest first.
func (t *transcript) tail(width int, s Style, want int) []string {
	bs := t.snapshot()
	var chunks [][]string
	n := 0
	for i := len(bs) - 1; i >= 0 && n < want; i-- {
		l := bs[i].lines(width, s, false)
		chunks = append(chunks, l)
		n += len(l)
	}
	var out []string
	for i := len(chunks) - 1; i >= 0; i-- {
		out = append(out, chunks[i]...)
	}
	return out
}

// rawBlock is output the program printed: lines of text, already styled.
type rawBlock struct{ text string }

func (b *rawBlock) lines(width int, _ Style, _ bool) []string {
	var out []string
	for _, l := range strings.Split(b.text, "\n") {
		out = append(out, hardWrap(l, width)...)
	}
	return out
}

// rowsBlock is rows rendered once, at the width they were drawn.
type rowsBlock struct{ rows []string }

func (b *rowsBlock) lines(width int, _ Style, _ bool) []string {
	var out []string
	for _, r := range b.rows {
		out = append(out, hardWrap(r, width)...)
	}
	return out
}

// promptBlock is a prompt as the person submitted it.
type promptBlock struct {
	text   string
	prompt string
}

func (b *promptBlock) lines(width int, s Style, _ bool) []string {
	text := sanitize(b.text, false)
	pw := displayWidth(b.prompt)
	indent := strings.Repeat(" ", pw)
	var out []string
	for i, l := range strings.Split(text, "\n") {
		lead := indent
		if i == 0 {
			lead = b.prompt
		}
		for j, r := range wrapWords(l, max(width-pw, 8)) {
			if j > 0 {
				lead = indent
			}
			out = append(out, lead+s.Bold(r))
		}
	}
	return out
}

// wrapWords wraps plain text at word boundaries to width columns; a word
// longer than a row is the only thing ever cut.
func wrapWords(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	if displayWidth(text) <= width {
		return []string{text}
	}
	var rows []string
	var row strings.Builder
	used := 0
	for _, word := range splitKeepSpaces(text) {
		ww := displayWidth(word)
		isSpace := strings.TrimSpace(word) == ""
		switch {
		case used+ww <= width:
			row.WriteString(word)
			used += ww
		case isSpace:
			// A space at the end of a row is dropped with the break.
			rows = append(rows, strings.TrimRight(row.String(), " "))
			row.Reset()
			used = 0
		case ww > width:
			for _, part := range breakWord(word, width) {
				if used > 0 && used+displayWidth(part) > width {
					rows = append(rows, strings.TrimRight(row.String(), " "))
					row.Reset()
					used = 0
				}
				row.WriteString(part)
				used += displayWidth(part)
			}
		default:
			rows = append(rows, strings.TrimRight(row.String(), " "))
			row.Reset()
			row.WriteString(word)
			used = ww
		}
	}
	rows = append(rows, strings.TrimRight(row.String(), " "))
	return rows
}

// splitKeepSpaces splits text into words and the runs of spaces between them.
func splitKeepSpaces(text string) []string {
	var out []string
	start := 0
	inSpace := false
	for i, r := range text {
		sp := r == ' '
		if i > 0 && sp != inSpace {
			out = append(out, text[start:i])
			start = i
		}
		inSpace = sp
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}

// breakWord cuts a word wider than a row into pieces of at most width
// columns, after a slash or a dash where one falls near the end of a piece,
// so a long path breaks between its parts.
func breakWord(word string, width int) []string {
	var out []string
	for displayWidth(word) > width {
		rows := hardWrap(word, width)
		piece := rows[0]
		if i := strings.LastIndexAny(piece, "/-_."); i >= width/2 && i < len(piece)-1 {
			piece = piece[:i+1]
		}
		out = append(out, piece)
		word = word[len(piece):]
	}
	return append(out, word)
}

// viewBlock wraps something the terminal draws in the Block the Surface
// carries.
func viewBlock(v block) Block { return Block{Kind: BlockNotice, view: v} }

// blockView is how the terminal draws b: its own view, or one made from
// its kind and text.
func blockView(b Block) block {
	if b.view != nil {
		return b.view
	}
	return &surfaceBlock{b: b}
}

// surfaceBlock draws a Block given by kind and text.
type surfaceBlock struct{ b Block }

func (sb *surfaceBlock) lines(width int, s Style, expanded bool) []string {
	b := sb.b
	text := sanitize(b.Text, false)
	var out []string
	if b.Path != "" && (b.Kind == BlockDiff || b.Kind == BlockToolOut) {
		out = append(out, s.Bold(sanitize(b.Path, false)))
	}
	switch b.Kind {
	case BlockError:
		for i, l := range wrapWords(text, max(width-2, 8)) {
			lead := s.Red("✕ ")
			if i > 0 {
				lead = "  "
			}
			out = append(out, lead+l)
		}
	case BlockNotice:
		for _, line := range strings.Split(text, "\n") {
			for _, l := range wrapWords(line, width) {
				out = append(out, s.Dim(l))
			}
		}
	case BlockMarkdown:
		out = append(out, renderMarkdown(s, text, width)...)
	case BlockTable:
		if len(b.Rows) > 0 {
			rows := make([][]string, len(b.Rows))
			for i, r := range b.Rows {
				for _, c := range r {
					rows[i] = append(rows[i], sanitize(c, false))
				}
			}
			out = append(out, renderTable(s, rows, width)...)
		}
	case BlockDiff:
		for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			for _, row := range hardWrap(l, width) {
				switch {
				case strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++"):
					row = s.DiffAdd(row)
				case strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---"):
					row = s.DiffDel(row)
				case strings.HasPrefix(l, "@@"):
					row = s.Cyan(row)
				}
				out = append(out, row)
			}
		}
	default:
		rb := &resultBlock{body: strings.Split(strings.TrimRight(text, "\n"), "\n"), headN: 3, tailN: 2}
		out = append(out, rb.lines(width, s, expanded)...)
	}
	return out
}
