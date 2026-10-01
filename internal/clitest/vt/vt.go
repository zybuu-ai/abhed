// Package vt is a small terminal emulator for tests: the subset of xterm
// the abhed CLI and common terminals use, enough to say what a person would
// see. It is test-only and has no dependency beyond x/text.
//
// It keeps a grid of cells with their style, the cursor, a scroll region,
// the alternate screen and the lines scrolled off the top. It answers the
// queries a program makes of its terminal (cursor position, device
// attributes, mode reports, background colour) through a reply function,
// and remembers titles, clipboard writes and notifications.
package vt

import (
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/text/width"
)

// Attrs is a cell's style. FG and BG are "" for the default, a palette
// index ("1", "202") or "#rrggbb".
type Attrs struct {
	FG, BG                                string
	Bold, Dim, Italic, Underline, Reverse bool
}

// Cell is one character cell. Width is 2 for a wide character's first
// cell and 0 for its second; a blank cell has Rune ' ' and Width 1.
type Cell struct {
	Rune  rune
	Width int
	Attrs Attrs
}

var blank = Cell{Rune: ' ', Width: 1}

type grid struct {
	cells [][]Cell
}

func newGrid(cols, rows int) *grid {
	g := &grid{cells: make([][]Cell, rows)}
	for i := range g.cells {
		g.cells[i] = blankRow(cols, Attrs{})
	}
	return g
}

func blankRow(cols int, a Attrs) []Cell {
	r := make([]Cell, cols)
	for i := range r {
		r[i] = Cell{Rune: ' ', Width: 1, Attrs: Attrs{BG: a.BG}}
	}
	return r
}

type cursor struct {
	x, y  int
	attrs Attrs
	wrap  bool
}

// Modes are the DEC private modes a test asks about.
type Modes struct {
	CursorVisible  bool
	BracketedPaste bool
	SyncOutput     bool // mode 2026 set by the program
	AltScreen      bool
	AppCursor      bool
	AutoWrap       bool
	FocusEvents    bool
}

// Terminal is the emulator. It is safe for one writer and many readers.
type Terminal struct {
	mu         sync.Mutex
	cols, rows int
	main, alt  *grid
	g          *grid
	cur        cursor
	saved      [2]cursor // main, alt
	top, bot   int       // scroll region, inclusive
	modes      Modes

	title         string
	clipboard     []string
	notifications []string
	scrollback    []string
	// Changes counts writes that changed a cell or the cursor.
	changes uint64

	// NoSync makes the terminal report mode 2026 as unsupported.
	NoSync bool
	// Background answers an OSC 11 query; "" means rgb:0000/0000/0000.
	Background string
	reply      func([]byte)

	st     pstate
	params []byte
	inter  []byte
	priv   byte
	osc    []byte
	utf    []byte
	// maxScrollback bounds the kept scrollback.
	maxScrollback int
}

type pstate int

const (
	sGround pstate = iota
	sEsc
	sEscInter
	sCSI
	sOSC
	sOSCEsc
	sDCS
	sDCSEsc
)

// New returns a terminal of the given size. reply receives the answers to
// the program's queries; nil drops them.
func New(cols, rows int, reply func([]byte)) *Terminal {
	t := &Terminal{cols: cols, rows: rows, reply: reply, maxScrollback: 10000}
	t.main, t.alt = newGrid(cols, rows), newGrid(cols, rows)
	t.g = t.main
	t.top, t.bot = 0, rows-1
	t.modes.CursorVisible, t.modes.AutoWrap = true, true
	return t
}

// Write feeds the program's output to the terminal.
func (t *Terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, b := range p {
		t.byte(b)
	}
	return len(p), nil
}

// Changes counts writes that changed what is shown.
func (t *Terminal) Changes() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.changes
}

func (t *Terminal) send(s string) {
	if t.reply != nil {
		t.reply([]byte(s))
	}
}

func (t *Terminal) byte(b byte) {
	switch t.st {
	case sGround:
		t.ground(b)
	case sEsc:
		t.esc(b)
	case sEscInter:
		// ESC ( B and the like: one final byte after the intermediate.
		if b >= 0x30 && b <= 0x7e {
			t.st = sGround
		}
	case sCSI:
		switch {
		case b >= 0x30 && b <= 0x3f:
			if len(t.params) == 0 && (b == '?' || b == '>' || b == '<' || b == '=') {
				t.priv = b
			} else {
				t.params = append(t.params, b)
			}
		case b >= 0x20 && b <= 0x2f:
			t.inter = append(t.inter, b)
		case b >= 0x40 && b <= 0x7e:
			t.st = sGround
			t.csi(b)
		case b == 0x1b:
			t.st = sEsc
		case b < 0x20:
			t.control(b)
		}
	case sOSC:
		switch b {
		case 0x07:
			t.st = sGround
			t.oscDone()
		case 0x1b:
			t.st = sOSCEsc
		default:
			if len(t.osc) < 1<<20 {
				t.osc = append(t.osc, b)
			}
		}
	case sOSCEsc:
		t.st = sGround
		if b == '\\' {
			t.oscDone()
		}
	case sDCS:
		switch b {
		case 0x1b:
			t.st = sDCSEsc
		case 0x07:
			t.st = sGround
		}
	case sDCSEsc:
		t.st = sGround
	}
}

func (t *Terminal) ground(b byte) {
	if len(t.utf) > 0 || b >= 0x80 {
		t.utf = append(t.utf, b)
		if !utf8.FullRune(t.utf) {
			if len(t.utf) > 4 {
				t.utf = t.utf[:0]
			}
			return
		}
		r, _ := utf8.DecodeRune(t.utf)
		t.utf = t.utf[:0]
		t.print(r)
		return
	}
	switch {
	case b == 0x1b:
		t.st = sEsc
	case b < 0x20 || b == 0x7f:
		t.control(b)
	default:
		t.print(rune(b))
	}
}

func (t *Terminal) control(b byte) {
	switch b {
	case '\r':
		t.cur.x, t.cur.wrap = 0, false
	case '\n', 0x0b, 0x0c:
		t.index()
	case '\b':
		if t.cur.x > 0 {
			t.cur.x--
		}
		t.cur.wrap = false
	case '\t':
		next := (t.cur.x/8 + 1) * 8
		t.cur.x = min(next, t.cols-1)
		t.cur.wrap = false
	default:
		return
	}
	t.changes++
}

func (t *Terminal) esc(b byte) {
	t.st = sGround
	switch b {
	case '[':
		t.st, t.params, t.inter, t.priv = sCSI, t.params[:0], t.inter[:0], 0
	case ']':
		t.st, t.osc = sOSC, t.osc[:0]
	case 'P', 'X', '^', '_':
		t.st = sDCS
	case '(', ')', '*', '+', '#', '%':
		t.st = sEscInter
	case '7':
		t.saved[t.which()] = t.cur
	case '8':
		t.cur = t.saved[t.which()]
		t.changes++
	case 'D':
		t.index()
	case 'E':
		t.cur.x = 0
		t.index()
	case 'M':
		t.reverseIndex()
	case 'c':
		reply, noSync, bg := t.reply, t.NoSync, t.Background
		*t = *New(t.cols, t.rows, reply)
		t.NoSync, t.Background = noSync, bg
	}
}

func (t *Terminal) which() int {
	if t.g == t.alt {
		return 1
	}
	return 0
}

func (t *Terminal) print(r rune) {
	w := runeWidth(r)
	if w == 0 {
		return // combining marks and zero-width: not modelled
	}
	if t.cur.wrap {
		if t.modes.AutoWrap {
			t.cur.x = 0
			t.index()
		}
		t.cur.wrap = false
	}
	if w == 2 && t.cur.x == t.cols-1 {
		// A wide character does not fit in the last column: wrap first.
		t.set(t.cur.y, t.cur.x, blank)
		if t.modes.AutoWrap {
			t.cur.x = 0
			t.index()
		}
	}
	row := t.g.cells[t.cur.y]
	// Overwriting half of a wide character blanks the other half.
	if row[t.cur.x].Width == 0 && t.cur.x > 0 {
		row[t.cur.x-1] = Cell{Rune: ' ', Width: 1, Attrs: row[t.cur.x-1].Attrs}
	}
	if w == 1 && row[t.cur.x].Width == 2 && t.cur.x+1 < t.cols {
		row[t.cur.x+1] = Cell{Rune: ' ', Width: 1, Attrs: row[t.cur.x+1].Attrs}
	}
	row[t.cur.x] = Cell{Rune: r, Width: w, Attrs: t.cur.attrs}
	if w == 2 && t.cur.x+1 < t.cols {
		row[t.cur.x+1] = Cell{Rune: 0, Width: 0, Attrs: t.cur.attrs}
	}
	t.changes++
	if t.cur.x+w >= t.cols {
		t.cur.x = t.cols - 1
		t.cur.wrap = true
		return
	}
	t.cur.x += w
}

func (t *Terminal) set(y, x int, c Cell) {
	if y >= 0 && y < t.rows && x >= 0 && x < t.cols {
		t.g.cells[y][x] = c
		t.changes++
	}
}

// runeWidth is a character's width in cells.
func runeWidth(r rune) int {
	switch {
	case r == 0 || r < 0x20 || (r >= 0x7f && r < 0xa0):
		return 0
	case r == 0x200b || r == 0x200c || r == 0x200d || r == 0xfe0f:
		return 0
	case r >= 0x0300 && r <= 0x036f:
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	// Emoji presentation characters most terminals draw two cells wide.
	if r >= 0x1f300 && r <= 0x1faff {
		return 2
	}
	return 1
}

// index moves the cursor down a line, scrolling the region at its bottom.
func (t *Terminal) index() {
	t.cur.wrap = false
	switch {
	case t.cur.y == t.bot:
		t.scrollUp(1)
	case t.cur.y < t.rows-1:
		t.cur.y++
	}
	t.changes++
}

func (t *Terminal) reverseIndex() {
	t.cur.wrap = false
	switch {
	case t.cur.y == t.top:
		t.scrollDown(1)
	case t.cur.y > 0:
		t.cur.y--
	}
	t.changes++
}

// scrollUp scrolls the region up n lines. Lines leaving the top of the
// whole main screen join the scrollback.
func (t *Terminal) scrollUp(n int) {
	n = min(n, t.bot-t.top+1)
	for i := 0; i < n; i++ {
		if t.g == t.main && t.top == 0 {
			t.scrollback = append(t.scrollback, rowText(t.g.cells[0]))
			if over := len(t.scrollback) - t.maxScrollback; over > 0 {
				t.scrollback = t.scrollback[over:]
			}
		}
		copy(t.g.cells[t.top:t.bot], t.g.cells[t.top+1:t.bot+1])
		t.g.cells[t.bot] = blankRow(t.cols, t.cur.attrs)
	}
	t.changes++
}

func (t *Terminal) scrollDown(n int) {
	n = min(n, t.bot-t.top+1)
	for i := 0; i < n; i++ {
		copy(t.g.cells[t.top+1:t.bot+1], t.g.cells[t.top:t.bot])
		t.g.cells[t.top] = blankRow(t.cols, t.cur.attrs)
	}
	t.changes++
}

// csiParams parses the numeric parameters, with def for missing ones.
// A sub-parameter after ':' is ignored.
func (t *Terminal) csiParams(def int) []int {
	parts := strings.Split(string(t.params), ";")
	out := make([]int, len(parts))
	for i, p := range parts {
		if j := strings.IndexByte(p, ':'); j >= 0 {
			p = p[:j]
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			n = def
		}
		out[i] = n
	}
	return out
}

func (t *Terminal) csi(final byte) {
	if len(t.inter) > 0 {
		t.csiInter(final)
		return
	}
	if t.priv == '?' {
		switch final {
		case 'h', 'l':
			for _, m := range t.csiParams(0) {
				t.privMode(m, final == 'h')
			}
		}
		return
	}
	if t.priv != 0 {
		// CSI > c (secondary attributes), CSI > 4;1 m and the like.
		if t.priv == '>' && final == 'c' {
			t.send("\x1b[>0;10;1c")
		}
		return
	}
	p := t.csiParams(1)
	n := max(p[0], 1)
	defer func() { t.changes++ }()
	switch final {
	case 'A':
		t.cur.y = max(t.cur.y-n, t.upLimit())
		t.cur.wrap = false
	case 'B', 'e':
		t.cur.y = min(t.cur.y+n, t.downLimit())
		t.cur.wrap = false
	case 'C', 'a':
		t.cur.x = min(t.cur.x+n, t.cols-1)
		t.cur.wrap = false
	case 'D':
		t.cur.x = max(t.cur.x-n, 0)
		t.cur.wrap = false
	case 'E':
		t.cur.y, t.cur.x = min(t.cur.y+n, t.downLimit()), 0
		t.cur.wrap = false
	case 'F':
		t.cur.y, t.cur.x = max(t.cur.y-n, t.upLimit()), 0
		t.cur.wrap = false
	case 'G', '`':
		t.cur.x = clamp(n-1, t.cols-1)
		t.cur.wrap = false
	case 'd':
		t.cur.y = clamp(n-1, t.rows-1)
		t.cur.wrap = false
	case 'H', 'f':
		row, col := 1, 1
		if len(p) > 0 && p[0] > 0 {
			row = p[0]
		}
		if len(p) > 1 && p[1] > 0 {
			col = p[1]
		}
		t.cur.y, t.cur.x = clamp(row-1, t.rows-1), clamp(col-1, t.cols-1)
		t.cur.wrap = false
	case 'J':
		t.eraseDisplay(t.csiParams(0)[0])
	case 'K':
		t.eraseLine(t.csiParams(0)[0])
	case '@':
		t.insertChars(n)
	case 'P':
		t.deleteChars(n)
	case 'X':
		row := t.g.cells[t.cur.y]
		for x := t.cur.x; x < min(t.cur.x+n, t.cols); x++ {
			row[x] = Cell{Rune: ' ', Width: 1, Attrs: Attrs{BG: t.cur.attrs.BG}}
		}
	case 'L':
		if t.cur.y >= t.top && t.cur.y <= t.bot {
			top := t.top
			t.top = t.cur.y
			t.scrollDown(n)
			t.top = top
		}
	case 'M':
		if t.cur.y >= t.top && t.cur.y <= t.bot {
			top := t.top
			t.top = t.cur.y
			// Deleted lines never reach the scrollback.
			g := t.g
			for i := 0; i < min(n, t.bot-t.top+1); i++ {
				copy(g.cells[t.top:t.bot], g.cells[t.top+1:t.bot+1])
				g.cells[t.bot] = blankRow(t.cols, t.cur.attrs)
			}
			t.top = top
		}
	case 'S':
		t.scrollUp(n)
	case 'T':
		t.scrollDown(n)
	case 'm':
		t.sgr()
	case 'r':
		top, bot := 1, t.rows
		if len(p) > 0 && p[0] > 0 {
			top = p[0]
		}
		if len(p) > 1 && p[1] > 0 {
			bot = p[1]
		}
		if top < bot && bot <= t.rows {
			t.top, t.bot = top-1, bot-1
			t.cur.x, t.cur.y, t.cur.wrap = 0, 0, false
		}
	case 's':
		t.saved[t.which()] = t.cur
	case 'u':
		t.cur = t.saved[t.which()]
	case 'n':
		switch t.csiParams(0)[0] {
		case 5:
			t.send("\x1b[0n")
		case 6:
			t.send("\x1b[" + strconv.Itoa(t.cur.y+1) + ";" + strconv.Itoa(t.cur.x+1) + "R")
		}
	case 'c':
		t.send("\x1b[?62;22c")
	case 't':
		if p[0] == 18 {
			t.send("\x1b[8;" + strconv.Itoa(t.rows) + ";" + strconv.Itoa(t.cols) + "t")
		}
	}
}

// csiInter handles the sequences with an intermediate byte: DECRQM
// (CSI ? Ps $ p) and DECSCUSR (CSI Ps SP q).
func (t *Terminal) csiInter(final byte) {
	if string(t.inter) == "$" && final == 'p' && t.priv == '?' {
		m := t.csiParams(0)[0]
		t.send("\x1b[?" + strconv.Itoa(m) + ";" + strconv.Itoa(t.modeReport(m)) + "$y")
	}
}

// modeReport is DECRPM's answer: 0 not recognised, 1 set, 2 reset.
func (t *Terminal) modeReport(m int) int {
	on := func(b bool) int {
		if b {
			return 1
		}
		return 2
	}
	switch m {
	case 2026:
		if t.NoSync {
			return 0
		}
		return on(t.modes.SyncOutput)
	case 2004:
		return on(t.modes.BracketedPaste)
	case 25:
		return on(t.modes.CursorVisible)
	case 1049, 1047, 47:
		return on(t.modes.AltScreen)
	case 1:
		return on(t.modes.AppCursor)
	case 7:
		return on(t.modes.AutoWrap)
	case 1004:
		return on(t.modes.FocusEvents)
	}
	return 0
}

func (t *Terminal) privMode(m int, on bool) {
	switch m {
	case 1:
		t.modes.AppCursor = on
	case 7:
		t.modes.AutoWrap = on
	case 25:
		t.modes.CursorVisible = on
	case 1004:
		t.modes.FocusEvents = on
	case 2004:
		t.modes.BracketedPaste = on
	case 2026:
		if !t.NoSync {
			t.modes.SyncOutput = on
		}
	case 47, 1047, 1049:
		if on == t.modes.AltScreen {
			return
		}
		if on {
			if m == 1049 {
				t.saved[0] = t.cur
			}
			t.alt = newGrid(t.cols, t.rows)
			t.g = t.alt
		} else {
			t.g = t.main
			if m == 1049 {
				t.cur = t.saved[0]
			}
		}
		t.modes.AltScreen = on
		t.changes++
	}
}

func (t *Terminal) upLimit() int {
	if t.cur.y >= t.top {
		return t.top
	}
	return 0
}

func (t *Terminal) downLimit() int {
	if t.cur.y <= t.bot {
		return t.bot
	}
	return t.rows - 1
}

func (t *Terminal) eraseDisplay(mode int) {
	switch mode {
	case 0:
		t.eraseLine(0)
		for y := t.cur.y + 1; y < t.rows; y++ {
			t.g.cells[y] = blankRow(t.cols, t.cur.attrs)
		}
	case 1:
		t.eraseLine(1)
		for y := 0; y < t.cur.y; y++ {
			t.g.cells[y] = blankRow(t.cols, t.cur.attrs)
		}
	case 2:
		for y := 0; y < t.rows; y++ {
			t.g.cells[y] = blankRow(t.cols, t.cur.attrs)
		}
	case 3:
		t.scrollback = nil
	}
}

func (t *Terminal) eraseLine(mode int) {
	row := t.g.cells[t.cur.y]
	from, to := 0, t.cols
	switch mode {
	case 0:
		from = t.cur.x
	case 1:
		to = t.cur.x + 1
	}
	for x := from; x < min(to, t.cols); x++ {
		row[x] = Cell{Rune: ' ', Width: 1, Attrs: Attrs{BG: t.cur.attrs.BG}}
	}
	t.cur.wrap = false
}

func (t *Terminal) insertChars(n int) {
	row := t.g.cells[t.cur.y]
	n = min(n, t.cols-t.cur.x)
	copy(row[t.cur.x+n:], row[t.cur.x:t.cols-n])
	for x := t.cur.x; x < t.cur.x+n; x++ {
		row[x] = blank
	}
}

func (t *Terminal) deleteChars(n int) {
	row := t.g.cells[t.cur.y]
	n = min(n, t.cols-t.cur.x)
	copy(row[t.cur.x:], row[t.cur.x+n:])
	for x := t.cols - n; x < t.cols; x++ {
		row[x] = blank
	}
}

// sgr applies Select Graphic Rendition.
func (t *Terminal) sgr() {
	p := t.csiParams(0)
	a := &t.cur.attrs
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == 0:
			*a = Attrs{}
		case c == 1:
			a.Bold = true
		case c == 2:
			a.Dim = true
		case c == 3:
			a.Italic = true
		case c == 4:
			a.Underline = true
		case c == 7:
			a.Reverse = true
		case c == 22:
			a.Bold, a.Dim = false, false
		case c == 23:
			a.Italic = false
		case c == 24:
			a.Underline = false
		case c == 27:
			a.Reverse = false
		case c >= 30 && c <= 37:
			a.FG = strconv.Itoa(c - 30)
		case c == 39:
			a.FG = ""
		case c >= 40 && c <= 47:
			a.BG = strconv.Itoa(c - 40)
		case c == 49:
			a.BG = ""
		case c >= 90 && c <= 97:
			a.FG = strconv.Itoa(c - 90 + 8)
		case c >= 100 && c <= 107:
			a.BG = strconv.Itoa(c - 100 + 8)
		case c == 38 || c == 48:
			var col string
			switch {
			case i+2 < len(p) && p[i+1] == 5:
				col = strconv.Itoa(p[i+2])
				i += 2
			case i+4 < len(p) && p[i+1] == 2:
				col = "#" + hex2(p[i+2]) + hex2(p[i+3]) + hex2(p[i+4])
				i += 4
			default:
				i = len(p)
				continue
			}
			if c == 38 {
				a.FG = col
			} else {
				a.BG = col
			}
		}
	}
}

func hex2(n int) string {
	const digits = "0123456789abcdef"
	n = clamp(n, 255)
	return string([]byte{digits[n>>4], digits[n&15]})
}

func (t *Terminal) oscDone() {
	s := string(t.osc)
	num, rest, _ := strings.Cut(s, ";")
	switch num {
	case "0", "2":
		t.title = rest
	case "52":
		// OSC 52 ; c ; base64 — kept as sent.
		if _, data, ok := strings.Cut(rest, ";"); ok && data != "?" {
			t.clipboard = append(t.clipboard, data)
		}
	case "9":
		t.notifications = append(t.notifications, rest)
	case "777":
		t.notifications = append(t.notifications, rest)
	case "11":
		if rest == "?" {
			bg := t.Background
			if bg == "" {
				bg = "rgb:0000/0000/0000"
			}
			t.send("\x1b]11;" + bg + "\x1b\\")
		}
	}
}

// Resize changes the size, keeping what fits, as terminals do on a window
// change. Rows lost from the top join the scrollback.
func (t *Terminal) Resize(cols, rows int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Shrinking keeps the cursor's row on screen; rows leaving the top of
	// the main screen join the scrollback.
	drop := max(0, t.cur.y+1-rows)
	fit := func(g *grid) *grid {
		n := newGrid(cols, rows)
		src := g.cells
		if g == t.g && drop > 0 {
			if g == t.main {
				for _, r := range src[:drop] {
					t.scrollback = append(t.scrollback, rowText(r))
				}
			}
			src = src[drop:]
		}
		for y := 0; y < min(rows, len(src)); y++ {
			copy(n.cells[y], src[y][:min(cols, len(src[y]))])
			if cols < len(src[y]) && n.cells[y][cols-1].Width == 2 {
				n.cells[y][cols-1] = blank
			}
		}
		return n
	}
	active := t.g == t.alt
	main, alt := fit(t.main), fit(t.alt)
	t.cur.y -= drop
	t.main, t.alt = main, alt
	t.g = t.main
	if active {
		t.g = t.alt
	}
	t.cols, t.rows = cols, rows
	t.top, t.bot = 0, rows-1
	t.cur.x, t.cur.y = clamp(t.cur.x, cols-1), clamp(t.cur.y, rows-1)
	t.cur.wrap = false
	t.changes++
}

// Snapshot is a copy of the screen at one moment.
type Snapshot struct {
	cols, rows    int
	cells         [][]Cell
	cx, cy        int
	title         string
	Modes         Modes
	Scrollback    []string
	Clipboard     []string
	Notifications []string
}

// Snapshot copies the screen.
func (t *Terminal) Snapshot() *Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := &Snapshot{cols: t.cols, rows: t.rows, cx: t.cur.x, cy: t.cur.y, title: t.title, Modes: t.modes,
		Scrollback:    append([]string(nil), t.scrollback...),
		Clipboard:     append([]string(nil), t.clipboard...),
		Notifications: append([]string(nil), t.notifications...)}
	s.cells = make([][]Cell, t.rows)
	for y := range t.g.cells {
		s.cells[y] = append([]Cell(nil), t.g.cells[y]...)
	}
	return s
}

func (s *Snapshot) Cols() int { return s.cols }
func (s *Snapshot) Rows() int { return s.rows }

// Line is row i's text with trailing spaces trimmed.
func (s *Snapshot) Line(i int) string {
	if i < 0 || i >= s.rows {
		return ""
	}
	return rowText(s.cells[i])
}

// Text is every row joined by newlines, trailing blank rows kept.
func (s *Snapshot) Text() string {
	lines := make([]string, s.rows)
	for i := range lines {
		lines[i] = s.Line(i)
	}
	return strings.Join(lines, "\n")
}

// Contains reports whether text appears on any one row.
func (s *Snapshot) Contains(text string) bool {
	for i := 0; i < s.rows; i++ {
		if strings.Contains(s.Line(i), text) {
			return true
		}
	}
	return false
}

// Cell is the cell at a row and column; outside the screen it is blank.
func (s *Snapshot) Cell(row, col int) Cell {
	if row < 0 || row >= s.rows || col < 0 || col >= s.cols {
		return blank
	}
	return s.cells[row][col]
}

func (s *Snapshot) Cursor() (row, col int) { return s.cy, s.cx }
func (s *Snapshot) Title() string          { return s.title }

func rowText(r []Cell) string {
	var b strings.Builder
	for _, c := range r {
		if c.Width == 0 {
			continue
		}
		if c.Rune == 0 {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(c.Rune)
	}
	return strings.TrimRight(b.String(), " ")
}

// clamp is v held within 0..hi.
func clamp(v, hi int) int {
	return max(0, min(v, hi))
}
