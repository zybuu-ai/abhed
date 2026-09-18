package ui

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"
)

// editor reads a line in raw mode with a live suggestion menu.
//
// This replaces golang.org/x/term's Terminal for the interactive path. That
// package cannot support the menu: its AutoCompleteCallback fires only from
// the `default` branch of its key switch, with the line as it was BEFORE the
// key was applied, and Backspace, Up, Down, Left and Right are all handled in
// earlier cases that return before the callback runs.
//
// The consequences were visible: deleting the last character left the menu on
// screen (the callback saw the pre-delete line, which still began with "/"),
// and arrow keys could not move through the suggestions because the callback
// never saw them. Both are structural, not tuning.
//
// Scope is deliberately the keys a prompt actually needs. This is not a
// readline: no kill ring, no reverse search, no multi-line editing.
type editor struct {
	in     io.Reader
	out    io.Writer
	prompt string

	line []rune
	pos  int // cursor index within line

	history []string
	hpos    int    // index into history while browsing; len(history) means "current"
	saved   string // the in-progress line, kept while browsing history

	// quiet suppresses the prompt and menu while a turn is running. The reader
	// goroutine keeps reading so a steering message can be typed mid-run, but
	// it must not paint a prompt over the turn's output — that is what stacked
	// a column of prompt glyphs under each answer.
	quiet bool

	// reading is true only between the start of readLine and the line being
	// submitted. Outside that window the editor draws nothing, so a turn's
	// output — and its thinking indicator — has the screen to itself.
	reading bool

	// menu state
	menu     []Command
	menuSel  int // -1 when nothing is selected
	menuRows int // rows currently drawn, so they can be erased

	mu sync.Mutex
}

func newEditor(in io.Reader, out io.Writer, prompt string) *editor {
	return &editor{in: in, out: out, prompt: prompt, menuSel: -1}
}

const (
	keyCtrlA = 1
	keyCtrlB = 2
	keyCtrlC = 3
	keyCtrlD = 4
	keyCtrlE = 5
	keyCtrlF = 6
	keyCtrlK = 11
	keyCtrlN = 14
	keyCtrlP = 16
	keyCtrlU = 21
	keyCtrlW = 23
	keyEnter = 13
	keyTab   = 9
	keyEsc   = 27
	keyDel   = 127
)

// readLine returns the next line, or io.EOF on Ctrl-D with an empty line.
func (e *editor) readLine() (string, error) {
	e.line = e.line[:0]
	e.pos = 0
	e.hpos = len(e.history)
	e.menuSel = -1
	e.mu.Lock()
	e.reading = true
	e.mu.Unlock()
	e.redraw()

	// Whatever ends the read — Enter, Ctrl-C, Ctrl-D, an error — the editor
	// stops owning the line.
	defer func() {
		e.mu.Lock()
		e.reading = false
		e.mu.Unlock()
	}()

	var buf [1]byte
	for {
		n, err := e.in.Read(buf[:])
		if err != nil {
			e.clearMenu()
			return "", err
		}
		if n == 0 {
			continue
		}
		k := rune(buf[0])

		switch k {
		case keyEnter:
			// A highlighted suggestion is what Enter accepts: it fills the line
			// and SUBMITS it, rather than putting the text there and waiting
			// for a second Enter. Choosing from a list is one gesture.
			//
			// Commands that take an argument are the exception — they fill the
			// line and wait, because submitting "/mode" with no mode is not
			// what the user meant by choosing it.
			if e.menuSel >= 0 && e.menuSel < len(e.menu) {
				c := e.menu[e.menuSel]
				e.acceptSuggestion()
				if c.Args != "" {
					continue // wait for the argument
				}
			}
			e.clearMenu()
			fmt.Fprint(e.out, "\r\n")
			out := string(e.line)
			if s := strings.TrimSpace(out); s != "" {
				e.history = append(e.history, out)
			}
			return out, nil

		case keyCtrlC:
			e.clearMenu()
			fmt.Fprint(e.out, "\r\n")
			return "", errInterrupted

		case keyCtrlD:
			if len(e.line) == 0 {
				e.clearMenu()
				fmt.Fprint(e.out, "\r\n")
				return "", io.EOF
			}
			e.deleteForward()

		case keyTab:
			e.complete()

		case keyDel, 8:
			e.backspace()

		case keyCtrlA:
			e.pos = 0
			e.redraw()
		case keyCtrlE:
			e.pos = len(e.line)
			e.redraw()
		case keyCtrlB:
			e.moveLeft()
		case keyCtrlF:
			e.moveRight()
		case keyCtrlP:
			e.historyPrev()
		case keyCtrlN:
			e.historyNext()
		case keyCtrlU:
			e.line = append([]rune{}, e.line[e.pos:]...)
			e.pos = 0
			e.afterEdit()
		case keyCtrlK:
			e.line = e.line[:e.pos]
			e.afterEdit()
		case keyCtrlW:
			e.deleteWord()

		case keyEsc:
			e.escape()

		default:
			if unicode.IsPrint(k) || k == '\t' {
				e.insert(k)
			}
		}
	}
}

// escape reads the rest of an escape sequence and dispatches the arrow keys.
//
// Up and Down drive the menu when one is open and history otherwise, which is
// the behaviour that makes the menu feel like a list rather than a poster.
func (e *editor) escape() {
	var b [2]byte
	if n, err := e.in.Read(b[:1]); err != nil || n == 0 {
		return
	}
	if b[0] != '[' && b[0] != 'O' {
		return
	}
	if n, err := e.in.Read(b[1:2]); err != nil || n == 0 {
		return
	}
	switch b[1] {
	case 'A': // up
		if len(e.menu) > 0 {
			e.moveSelection(-1)
			return
		}
		e.historyPrev()
	case 'B': // down
		if len(e.menu) > 0 {
			e.moveSelection(+1)
			return
		}
		e.historyNext()
	case 'C':
		e.moveRight()
	case 'D':
		e.moveLeft()
	case 'H':
		e.pos = 0
		e.redraw()
	case 'F':
		e.pos = len(e.line)
		e.redraw()
	case '3': // Delete: consumes the trailing '~'
		var t [1]byte
		_, _ = e.in.Read(t[:])
		e.deleteForward()
	}
}

func (e *editor) insert(k rune) {
	e.line = append(e.line, 0)
	copy(e.line[e.pos+1:], e.line[e.pos:])
	e.line[e.pos] = k
	e.pos++
	e.afterEdit()
}

func (e *editor) backspace() {
	if e.pos == 0 {
		return
	}
	e.line = append(e.line[:e.pos-1], e.line[e.pos:]...)
	e.pos--
	e.afterEdit()
}

func (e *editor) deleteForward() {
	if e.pos >= len(e.line) {
		return
	}
	e.line = append(e.line[:e.pos], e.line[e.pos+1:]...)
	e.afterEdit()
}

func (e *editor) deleteWord() {
	i := e.pos
	for i > 0 && e.line[i-1] == ' ' {
		i--
	}
	for i > 0 && e.line[i-1] != ' ' {
		i--
	}
	e.line = append(e.line[:i], e.line[e.pos:]...)
	e.pos = i
	e.afterEdit()
}

func (e *editor) moveLeft() {
	if e.pos > 0 {
		e.pos--
		e.redraw()
	}
}

func (e *editor) moveRight() {
	if e.pos < len(e.line) {
		e.pos++
		e.redraw()
	}
}

func (e *editor) historyPrev() {
	if len(e.history) == 0 || e.hpos == 0 {
		return
	}
	if e.hpos == len(e.history) {
		e.saved = string(e.line)
	}
	e.hpos--
	e.setLine(e.history[e.hpos])
}

func (e *editor) historyNext() {
	if e.hpos >= len(e.history) {
		return
	}
	e.hpos++
	if e.hpos == len(e.history) {
		e.setLine(e.saved)
		return
	}
	e.setLine(e.history[e.hpos])
}

func (e *editor) setLine(s string) {
	e.line = []rune(s)
	e.pos = len(e.line)
	e.afterEdit()
}

// afterEdit recomputes the menu from the line as it now is.
//
// This is the fix for the menu that would not go away: it runs AFTER every
// edit, on the current line, so deleting the last character re-evaluates and
// finds nothing to show.
func (e *editor) afterEdit() {
	word := strings.TrimSpace(string(e.line))
	if strings.HasPrefix(word, "/") && !strings.ContainsAny(word, " \t") {
		next := MatchCommands(word)
		// Keep the highlight on the same command when the list is narrowing.
		if e.menuSel >= 0 && e.menuSel < len(e.menu) {
			want := e.menu[e.menuSel].Name
			e.menuSel = -1
			for i, c := range next {
				if c.Name == want {
					e.menuSel = i
					break
				}
			}
		}
		e.menu = next
	} else {
		e.menu = nil
		e.menuSel = -1
	}
	e.redraw()
}

func (e *editor) moveSelection(d int) {
	if len(e.menu) == 0 {
		return
	}
	switch {
	case e.menuSel < 0 && d > 0:
		e.menuSel = 0
	case e.menuSel < 0:
		e.menuSel = len(e.menu) - 1
	default:
		e.menuSel += d
		if e.menuSel < 0 {
			e.menuSel = len(e.menu) - 1
		}
		if e.menuSel >= len(e.menu) {
			e.menuSel = 0
		}
	}
	e.redraw()
}

// acceptSuggestion puts the highlighted command on the line.
func (e *editor) acceptSuggestion() {
	c := e.menu[e.menuSel]
	out := c.Name
	if c.Args != "" {
		out += " "
	}
	e.menu = nil
	e.menuSel = -1
	e.setLine(out)
}

// complete is Tab: take the highlight if there is one, otherwise complete as
// far as the candidates unambiguously allow.
func (e *editor) complete() {
	if len(e.menu) == 0 {
		return
	}
	if e.menuSel >= 0 {
		e.acceptSuggestion()
		return
	}
	if len(e.menu) == 1 {
		e.menuSel = 0
		e.acceptSuggestion()
		return
	}
	if p := CommonPrefix(e.menu); len(p) > len(strings.TrimSpace(string(e.line))) {
		e.setLine(p)
		return
	}
	// Ambiguous and already at the shared stem: highlight the first, so Tab
	// twice starts choosing rather than doing nothing.
	e.menuSel = 0
	e.redraw()
}

const menuMax = 8

// redraw paints the prompt, the line, and the menu beneath it.
//
// Everything below the prompt is erased and rewritten each time, then the
// cursor returns to its column. Redrawing the whole block is what keeps the
// menu in step with the line: partial updates were what left stale rows.
func (e *editor) redraw() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.quiet {
		return // a turn owns the screen
	}

	var b strings.Builder
	b.WriteString("\r\033[J") // column 0, erase to end of screen
	b.WriteString(e.prompt)
	b.WriteString(string(e.line))

	rows := 0
	if len(e.menu) > 0 {
		shown := e.menu
		more := 0
		if len(shown) > menuMax {
			// Keep the highlight visible when it moves past the window.
			start := 0
			if e.menuSel >= menuMax {
				start = e.menuSel - menuMax + 1
			}
			end := start + menuMax
			if end > len(shown) {
				end = len(shown)
			}
			more = len(shown) - (end - start)
			shown = shown[start:end]
		}
		s := NewStyle(e.out)
		for i, c := range shown {
			left := c.Name
			if c.Args != "" {
				left += " " + c.Args
			}
			selected := e.menuSel >= 0 && i+indexOffset(e.menu, shown) == e.menuSel
			row := fmt.Sprintf("%-18s  %s", left, c.Help)
			if selected {
				b.WriteString("\r\n  " + s.Reverse(" "+row+" "))
			} else {
				b.WriteString("\r\n  " + s.Cyan(fmt.Sprintf("%-18s", left)) + "  " + s.Dim(c.Help))
			}
			rows++
		}
		if more > 0 {
			b.WriteString("\r\n  " + s.Dim(fmt.Sprintf("… %d more", more)))
			rows++
		}
	}

	// Back up to the prompt line and place the cursor after the typed text.
	if rows > 0 {
		fmt.Fprintf(&b, "\033[%dA", rows)
	}
	fmt.Fprintf(&b, "\r\033[%dC", visibleLen(e.prompt)+e.pos)

	fmt.Fprint(e.out, b.String())
	e.menuRows = rows
}

// indexOffset maps a position in the visible window back to the full list.
func indexOffset(all, shown []Command) int {
	if len(all) == len(shown) || len(shown) == 0 {
		return 0
	}
	for i := range all {
		if all[i].Name == shown[0].Name {
			return i
		}
	}
	return 0
}

// clearMenu erases the rows below the prompt, used before handing the screen
// back to the caller.
func (e *editor) clearMenu() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.menuRows == 0 {
		return
	}
	fmt.Fprint(e.out, "\033[J")
	e.menu = nil
	e.menuSel = -1
	e.menuRows = 0
}

// setQuiet suspends or resumes prompt drawing. Resuming repaints, so the
// prompt reappears as soon as the turn that borrowed the screen is done.
func (e *editor) setQuiet(q bool) {
	e.mu.Lock()
	was := e.quiet
	e.quiet = q
	reading := e.reading
	e.mu.Unlock()
	if was && !q && reading {
		e.redraw()
	}
}

// setPrompt changes the prompt; the next redraw picks it up.
func (e *editor) setPrompt(p string) {
	e.mu.Lock()
	e.prompt = p
	e.mu.Unlock()
}

// write prints output above the line being edited.
//
// Only while a line is ACTUALLY being edited does this erase and repaint the
// prompt. While a turn is running the editor is not reading, and repainting
// there was actively harmful: the thinking indicator rewrites one line many
// times a second, and every frame was followed by a repainted prompt — which
// both hid the animation and left a column of stranded prompt glyphs behind.
//
// When no line is in progress this is a plain pass-through, which is what the
// renderer needs to own the screen for the duration of a turn.
func (e *editor) write(p []byte) (int, error) {
	e.mu.Lock()
	editing := e.reading
	if editing {
		fmt.Fprint(e.out, "\r\033[J") // drop the prompt and menu
	}
	e.mu.Unlock()

	n, err := rawWriter{e.out}.Write(p)

	if editing {
		e.redraw() // and put them back
	}
	return n, err
}

// visibleLen counts display columns, skipping ANSI escapes so a coloured
// prompt does not push the cursor too far right.
func visibleLen(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc && r == 'm':
			esc = false
		case esc:
		case r == 0x1b:
			esc = true
		default:
			n++
		}
	}
	return n
}

// errInterrupted is Ctrl-C on a line being typed: the line is abandoned and
// the prompt returns, which is not the same as ending the session.
var errInterrupted = errors.New("interrupted")

// ErrInterrupted reports whether a read ended in Ctrl-C.
func ErrInterrupted(err error) bool { return errors.Is(err, errInterrupted) }
