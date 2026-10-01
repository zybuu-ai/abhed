// Package vt is a small terminal emulator for tests: it applies the bytes a
// program writes to a grid of cells, as an xterm-compatible terminal would,
// so a test can assert on what a person would see — the screen, the
// scrollback and the cursor — rather than on escape sequences.
//
// It implements what the CLI and ordinary programs use: printing with
// autowrap and wide characters, cursor movement, erase, scrolling and
// scroll regions, SGR attributes, the alternate screen, and the private
// modes the dock sets. Anything else is parsed and ignored.
package vt

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/width"
)

// Cell is one column of the screen.
type Cell struct {
	Text  string // a grapheme cluster; "" for the second column of a wide one
	Wide  bool
	Attr  Attr
	Blank bool
}

// Attr is the styling of a cell.
type Attr struct {
	FG, BG                   string // "", a palette index, or "r;g;b"
	Bold, Dim, Italic, Under bool
	Reverse, Strike          bool
}

// Terminal is the emulator's state.
type Terminal struct {
	mu         sync.Mutex
	cols, rows int
	main, alt  [][]Cell
	grid       [][]Cell // main or alt
	altActive  bool
	x, y       int
	pending    bool // the last column was written: the next character wraps
	attr       Attr
	top, bot   int // scroll region, inclusive
	saved      [2]int
	savedAttr  Attr
	scrollback []string
	modes      map[string]bool
	// Written counts every byte applied.
	Written int64
	// Titles are the window titles set, in order.
	Titles []string

	partial []byte // an incomplete UTF-8 sequence or escape across writes
}

// New returns a terminal of cols by rows.
func New(cols, rows int) *Terminal {
	t := &Terminal{cols: cols, rows: rows, modes: map[string]bool{"?25": true, "?7": true}}
	t.main = blankGrid(cols, rows)
	t.alt = blankGrid(cols, rows)
	t.grid = t.main
	t.top, t.bot = 0, rows-1
	return t
}

func blankGrid(cols, rows int) [][]Cell {
	g := make([][]Cell, rows)
	for i := range g {
		g[i] = blankRow(cols)
	}
	return g
}

func blankRow(cols int) []Cell {
	r := make([]Cell, cols)
	for i := range r {
		r[i] = Cell{Text: " ", Blank: true}
	}
	return r
}

// Resize changes the size. Rows are kept from the top and cut or padded, as
// a terminal that does not reflow would; the cursor is clamped.
func (t *Terminal) Resize(cols, rows int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	resize := func(g [][]Cell, main bool) [][]Cell {
		// A shorter window keeps the cursor's row: rows above it go to
		// scrollback, and any further excess is cut from the bottom.
		if excess := len(g) - rows; excess > 0 {
			up := 0
			if main == !t.altActive {
				up = min(excess, max(0, t.y-(rows-1)))
			}
			for i := 0; i < up; i++ {
				if main {
					t.scrollback = append(t.scrollback, rowText(g[i]))
				}
			}
			g = g[up : up+rows]
			if main == !t.altActive {
				t.y -= up
			}
		}
		for len(g) < rows {
			g = append(g, blankRow(cols))
		}
		for i := range g {
			if len(g[i]) > cols {
				g[i] = g[i][:cols]
				if g[i][cols-1].Wide {
					g[i][cols-1] = Cell{Text: " ", Blank: true}
				}
			} else {
				for len(g[i]) < cols {
					g[i] = append(g[i], Cell{Text: " ", Blank: true})
				}
			}
		}
		return g
	}
	t.cols, t.rows = cols, rows
	t.main = resize(t.main, true)
	t.alt = resize(t.alt, false)
	if t.altActive {
		t.grid = t.alt
	} else {
		t.grid = t.main
	}
	t.top, t.bot = 0, rows-1
	t.x = min(max(t.x, 0), cols-1)
	t.y = min(max(t.y, 0), rows-1)
	t.pending = false
}

// Write applies p. It never fails.
func (t *Terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Written += int64(len(p))
	data := append(append([]byte(nil), t.partial...), p...)
	t.partial = nil
	i := 0
	for i < len(data) {
		c := data[i]
		switch {
		case c == 0x1b:
			n, ok := t.escape(data[i:])
			if !ok {
				t.partial = append([]byte(nil), data[i:]...)
				return len(p), nil
			}
			i += n
		case c < 0x20 || c == 0x7f:
			t.control(c)
			i++
		default:
			r, size := utf8.DecodeRune(data[i:])
			if r == utf8.RuneError && size == 1 && !utf8.FullRune(data[i:]) {
				t.partial = append([]byte(nil), data[i:]...)
				return len(p), nil
			}
			t.print(r)
			i += size
		}
	}
	return len(p), nil
}

func (t *Terminal) control(c byte) {
	switch c {
	case '\r':
		t.x, t.pending = 0, false
	case '\n', 0x0b, 0x0c:
		t.lineFeed()
	case '\b':
		if t.x > 0 {
			t.x--
		}
		t.pending = false
	case '\t':
		t.x = min((t.x/8+1)*8, t.cols-1)
	}
}

func (t *Terminal) lineFeed() {
	t.pending = false
	if t.y == t.bot {
		t.scrollUp(1)
		return
	}
	if t.y < t.rows-1 {
		t.y++
	}
}

func (t *Terminal) scrollUp(n int) {
	for ; n > 0; n-- {
		if t.top == 0 && !t.altActive {
			t.scrollback = append(t.scrollback, rowText(t.grid[0]))
		}
		copy(t.grid[t.top:t.bot], t.grid[t.top+1:t.bot+1])
		t.grid[t.bot] = blankRow(t.cols)
	}
}

func (t *Terminal) scrollDown(n int) {
	for ; n > 0; n-- {
		copy(t.grid[t.top+1:t.bot+1], t.grid[t.top:t.bot])
		t.grid[t.top] = blankRow(t.cols)
	}
}

// runeWidth mirrors what terminals draw: two columns for East Asian wide
// characters and emoji, none for marks that join the character before.
func runeWidth(r rune) int {
	switch {
	case r < 0x20:
		return 0
	case r < 0x300:
		return 1
	case joins(r):
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	if r >= 0x1f300 && r <= 0x1faff || r >= 0x1f000 && r <= 0x1f02f || r >= 0x1f0a0 && r <= 0x1f0ff {
		return 2
	}
	return 1
}

func joins(r rune) bool {
	switch {
	case r == 0x200d, r == 0x200c, r >= 0xfe00 && r <= 0xfe0f, r >= 0x1f3fb && r <= 0x1f3ff,
		r >= 0xe0020 && r <= 0xe007f, r == 0x20e3, r >= 0xe0100 && r <= 0xe01ef:
		return true
	}
	return r >= 0x300 && (unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Mc, r))
}

func regional(r rune) bool { return r >= 0x1f1e6 && r <= 0x1f1ff }

// prev is the cell a joining character attaches to.
func (t *Terminal) prev() (row, col int, ok bool) {
	col = t.x - 1
	if t.pending {
		col = t.x
	}
	row = t.y
	if col < 0 {
		return 0, 0, false
	}
	if t.grid[row][col].Text == "" && col > 0 {
		col--
	}
	return row, col, true
}

func (t *Terminal) print(r rune) {
	w := runeWidth(r)
	if pr, pc, ok := t.prev(); ok {
		c := &t.grid[pr][pc]
		last, _ := utf8.DecodeLastRuneInString(c.Text)
		first, _ := utf8.DecodeRuneInString(c.Text)
		join := w == 0 || last == 0x200d ||
			regional(r) && regional(first) && utf8.RuneCountInString(c.Text) == 1
		if join && !c.Blank {
			c.Text += string(r)
			if (r == 0xfe0f || r == 0x20e3 || regional(r)) && !c.Wide && pc+1 < t.cols {
				// Emoji presentation widens a narrow base, as terminals do.
				c.Wide = true
				t.grid[pr][pc+1] = Cell{Text: "", Attr: c.Attr}
				if !t.pending {
					t.x = min(t.x+1, t.cols-1)
					if pc+2 >= t.cols {
						t.pending = true
					}
				}
			}
			return
		}
	}
	if w == 0 {
		return
	}
	if t.pending || t.x+w > t.cols {
		if t.modes["?7"] {
			t.x = 0
			t.lineFeed()
		} else {
			t.x = t.cols - w
		}
	}
	t.pending = false
	row := t.grid[t.y]
	// Writing over half of a wide character clears the whole of it.
	if row[t.x].Text == "" && t.x > 0 {
		row[t.x-1] = Cell{Text: " ", Blank: true}
	}
	if row[t.x].Wide && t.x+1 < t.cols {
		row[t.x+1] = Cell{Text: " ", Blank: true}
	}
	row[t.x] = Cell{Text: string(r), Wide: w == 2, Attr: t.attr}
	if w == 2 {
		row[t.x+1] = Cell{Text: "", Attr: t.attr}
	}
	if t.x+w >= t.cols {
		t.x = t.cols - 1
		t.pending = true
	} else {
		t.x += w
	}
}

// escape applies the sequence at the start of b, returning its length, or
// ok=false when b ends before the sequence does.
func (t *Terminal) escape(b []byte) (int, bool) {
	if len(b) < 2 {
		return 0, false
	}
	switch b[1] {
	case '[':
		for i := 2; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				t.csi(string(b[2:i]), b[i])
				return i + 1, true
			}
		}
		return 0, false
	case ']', 'P', '_', '^':
		for i := 2; i < len(b); i++ {
			if b[i] == 0x07 {
				t.osc(string(b[2:i]))
				return i + 1, true
			}
			if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\' {
				t.osc(string(b[2:i]))
				return i + 2, true
			}
		}
		return 0, false
	case '7':
		t.saved, t.savedAttr = [2]int{t.x, t.y}, t.attr
	case '8':
		t.x, t.y, t.attr, t.pending = t.saved[0], t.saved[1], t.savedAttr, false
	case 'c':
		*t = *New(t.cols, t.rows)
	case 'M':
		if t.y == t.top {
			t.scrollDown(1)
		} else if t.y > 0 {
			t.y--
		}
	case 'D':
		t.lineFeed()
	case 'E':
		t.x = 0
		t.lineFeed()
	case '(', ')', '*', '+', '#', '%':
		if len(b) < 3 {
			return 0, false
		}
		return 3, true
	}
	return 2, true
}

func (t *Terminal) osc(s string) {
	if i := strings.IndexByte(s, ';'); i > 0 {
		switch s[:i] {
		case "0", "2":
			t.Titles = append(t.Titles, s[i+1:])
		}
	}
}

func (t *Terminal) csi(params string, final byte) {
	priv := ""
	if params != "" && strings.ContainsRune("?<>=", rune(params[0])) {
		priv, params = params[:1], params[1:]
	}
	inter := ""
	if n := len(params); n > 0 && (params[n-1] == '$' || params[n-1] == ' ' || params[n-1] == '"') {
		inter, params = params[n-1:], params[:n-1]
	}
	ps := strings.Split(params, ";")
	num := func(i, def int) int {
		if i >= len(ps) || ps[i] == "" {
			return def
		}
		n, err := strconv.Atoi(strings.SplitN(ps[i], ":", 2)[0])
		if err != nil {
			return def
		}
		return n
	}
	if inter != "" || priv == "<" || priv == ">" || priv == "=" {
		return // DECRQM, keyboard protocol and the like change nothing drawn
	}
	switch final {
	case 'A':
		t.y = max(t.y-max(num(0, 1), 1), 0)
		t.pending = false
	case 'B':
		t.y = min(t.y+max(num(0, 1), 1), t.rows-1)
		t.pending = false
	case 'C':
		t.x = min(t.x+max(num(0, 1), 1), t.cols-1)
		t.pending = false
	case 'D':
		t.x = max(t.x-max(num(0, 1), 1), 0)
		t.pending = false
	case 'E':
		t.y = min(t.y+max(num(0, 1), 1), t.rows-1)
		t.x, t.pending = 0, false
	case 'F':
		t.y = max(t.y-max(num(0, 1), 1), 0)
		t.x, t.pending = 0, false
	case 'G', '`':
		t.x = min(max(num(0, 1), 1), t.cols) - 1
		t.pending = false
	case 'd':
		t.y = min(max(num(0, 1), 1), t.rows) - 1
		t.pending = false
	case 'H', 'f':
		t.y = min(max(num(0, 1), 1), t.rows) - 1
		t.x = min(max(num(1, 1), 1), t.cols) - 1
		t.pending = false
	case 'J':
		switch num(0, 0) {
		case 0:
			t.eraseLine(t.y, t.x, t.cols)
			for r := t.y + 1; r < t.rows; r++ {
				t.grid[r] = blankRow(t.cols)
			}
		case 1:
			for r := 0; r < t.y; r++ {
				t.grid[r] = blankRow(t.cols)
			}
			t.eraseLine(t.y, 0, t.x+1)
		case 2:
			for r := range t.grid {
				t.grid[r] = blankRow(t.cols)
			}
		case 3:
			t.scrollback = nil
		}
	case 'K':
		switch num(0, 0) {
		case 0:
			t.eraseLine(t.y, t.x, t.cols)
		case 1:
			t.eraseLine(t.y, 0, t.x+1)
		case 2:
			t.eraseLine(t.y, 0, t.cols)
		}
	case 'X':
		t.eraseLine(t.y, t.x, min(t.x+max(num(0, 1), 1), t.cols))
	case 'P':
		n := max(num(0, 1), 1)
		row := t.grid[t.y]
		copy(row[t.x:], row[min(t.x+n, t.cols):])
		for i := max(t.cols-n, t.x); i < t.cols; i++ {
			row[i] = Cell{Text: " ", Blank: true}
		}
	case '@':
		n := max(num(0, 1), 1)
		row := t.grid[t.y]
		copy(row[min(t.x+n, t.cols):], row[t.x:])
		for i := t.x; i < min(t.x+n, t.cols); i++ {
			row[i] = Cell{Text: " ", Blank: true}
		}
	case 'L':
		if t.y >= t.top && t.y <= t.bot {
			top := t.top
			t.top = t.y
			t.scrollDown(max(num(0, 1), 1))
			t.top = top
		}
	case 'M':
		if t.y >= t.top && t.y <= t.bot {
			top := t.top
			t.top = t.y
			sb := t.scrollback
			t.scrollUp(max(num(0, 1), 1))
			t.scrollback = sb
			t.top = top
		}
	case 'S':
		t.scrollUp(max(num(0, 1), 1))
	case 'T':
		t.scrollDown(max(num(0, 1), 1))
	case 'r':
		t.top = min(max(num(0, 1), 1), t.rows) - 1
		t.bot = min(max(num(1, t.rows), 1), t.rows) - 1
		if t.top >= t.bot {
			t.top, t.bot = 0, t.rows-1
		}
		t.x, t.y, t.pending = 0, 0, false
	case 's':
		t.saved = [2]int{t.x, t.y}
	case 'u':
		t.x, t.y, t.pending = t.saved[0], t.saved[1], false
	case 'm':
		t.sgr(ps)
	case 'h', 'l':
		on := final == 'h'
		for _, p := range ps {
			key := priv + p
			t.modes[key] = on
			if key == "?1049" || key == "?1047" || key == "?47" {
				t.switchScreen(on)
			}
		}
	}
}

func (t *Terminal) switchScreen(alt bool) {
	if alt == t.altActive {
		return
	}
	if alt {
		t.saved, t.savedAttr = [2]int{t.x, t.y}, t.attr
		t.alt = blankGrid(t.cols, t.rows)
		t.grid = t.alt
		t.altActive = true
		return
	}
	t.grid = t.main
	t.altActive = false
	t.x, t.y, t.attr, t.pending = t.saved[0], t.saved[1], t.savedAttr, false
}

func (t *Terminal) eraseLine(row, from, to int) {
	for i := max(from, 0); i < min(to, t.cols); i++ {
		t.grid[row][i] = Cell{Text: " ", Blank: true, Attr: Attr{BG: t.attr.BG}}
	}
}

func (t *Terminal) sgr(ps []string) {
	for i := 0; i < len(ps); i++ {
		n, _ := strconv.Atoi(strings.SplitN(ps[i], ":", 2)[0])
		switch {
		case ps[i] == "" || n == 0:
			t.attr = Attr{}
		case n == 1:
			t.attr.Bold = true
		case n == 2:
			t.attr.Dim = true
		case n == 3:
			t.attr.Italic = true
		case n == 4:
			t.attr.Under = true
		case n == 7:
			t.attr.Reverse = true
		case n == 9:
			t.attr.Strike = true
		case n == 22:
			t.attr.Bold, t.attr.Dim = false, false
		case n == 23:
			t.attr.Italic = false
		case n == 24:
			t.attr.Under = false
		case n == 27:
			t.attr.Reverse = false
		case n == 29:
			t.attr.Strike = false
		case n >= 30 && n <= 37, n >= 90 && n <= 97:
			t.attr.FG = strconv.Itoa(n)
		case n == 39:
			t.attr.FG = ""
		case n >= 40 && n <= 47, n >= 100 && n <= 107:
			t.attr.BG = strconv.Itoa(n)
		case n == 49:
			t.attr.BG = ""
		case n == 38 || n == 48:
			var col string
			if i+2 < len(ps) && ps[i+1] == "5" {
				col = "5;" + ps[i+2]
				i += 2
			} else if i+4 < len(ps) && ps[i+1] == "2" {
				col = ps[i+2] + ";" + ps[i+3] + ";" + ps[i+4]
				i += 4
			}
			if n == 38 {
				t.attr.FG = col
			} else {
				t.attr.BG = col
			}
		}
	}
}

func rowText(r []Cell) string {
	var b strings.Builder
	for _, c := range r {
		b.WriteString(c.Text)
	}
	return strings.TrimRight(b.String(), " ")
}

// Lines returns the screen's rows as text, trailing spaces trimmed.
func (t *Terminal) Lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.grid))
	for i, r := range t.grid {
		out[i] = rowText(r)
	}
	return out
}

// Text is the screen as one string, trailing blank rows dropped.
func (t *Terminal) Text() string {
	lines := t.Lines()
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// Scrollback returns the rows that scrolled off the top of the main screen.
func (t *Terminal) Scrollback() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.scrollback...)
}

// All is the scrollback followed by the screen, trailing blank rows dropped.
func (t *Terminal) All() string {
	all := append(t.Scrollback(), t.Lines()...)
	for len(all) > 0 && all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}
	return strings.Join(all, "\n")
}

// Cursor returns the cursor's column and row.
func (t *Terminal) Cursor() (x, y int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.x, t.y
}

// Mode reports a mode set with h or l, such as "?2004" or "?1049".
func (t *Terminal) Mode(m string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.modes[m]
}

// CellAt returns the cell at column x, row y.
func (t *Terminal) CellAt(x, y int) Cell {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.grid[y][x]
}

// Size returns the columns and rows.
func (t *Terminal) Size() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cols, t.rows
}

// Dump renders the screen with a frame and the cursor, for failure messages.
func (t *Terminal) Dump() string {
	lines := t.Lines()
	x, y := t.Cursor()
	var b strings.Builder
	fmt.Fprintf(&b, "┌%s┐ cursor %d,%d\n", strings.Repeat("─", t.cols), x, y)
	for _, l := range lines {
		b.WriteString("│" + l + "\n")
	}
	b.WriteString("└" + strings.Repeat("─", t.cols) + "┘")
	return b.String()
}
