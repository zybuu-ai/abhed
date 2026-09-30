package ui

import (
	"io"
	"strconv"
	"strings"
)

// screen draws the live region: the rows at the bottom of the terminal that
// change in place (the input, the activity line, a dialog, the footer), with
// the finished transcript scrolling above them.
//
// It keeps the rows it drew last and, on each frame, rewrites only what
// changed: the rows that differ, and within a row only from the first cell
// that differs. A keystroke at the end of the line is one byte on the wire.
// Redrawing the whole dock on every key cost 800 to 1,400 bytes a key, which
// flickers on terminals without synchronized output (Terminal.app) and crawls
// over a slow link.
//
// Rows are never wider than the terminal minus one column. A row that fills
// the last column leaves the cursor in the terminal's pending-wrap state,
// which each terminal resolves differently; one column of slack makes every
// cursor move exact.
type screen struct {
	out io.Writer

	rows   []string // what the live region shows now
	cr, cc int      // where the terminal's cursor is, relative to the region
	extent int      // rows below the region's top known to exist on screen
	drawn  bool

	// written counts bytes sent, for the budgets the tests hold.
	written int64
	// buf collects a frame, sent in one write.
	buf strings.Builder
}

func newScreen(out io.Writer) *screen { return &screen{out: out} }

// flush sends the buffered bytes in one write, so a frame reaches the
// terminal whole.
func (s *screen) flush() {
	if s.buf.Len() == 0 {
		return
	}
	str := s.buf.String()
	s.buf.Reset()
	s.written += int64(len(str))
	_, _ = io.WriteString(s.out, str)
}

func (s *screen) csi(n int, final byte) {
	s.buf.WriteString("\x1b[")
	if n != 1 {
		s.buf.WriteString(strconv.Itoa(n))
	}
	s.buf.WriteByte(final)
}

// moveTo puts the cursor at row r, column c of the region. Rows past the
// region's known extent are made with newlines, which scroll the screen when
// the region is at the bottom; a cursor-down would stop at the last row.
func (s *screen) moveTo(r, c int) {
	switch {
	case r < s.cr:
		s.csi(s.cr-r, 'A')
	case r > s.cr:
		if r < s.extent {
			s.csi(r-s.cr, 'B')
		} else {
			if s.extent-1 > s.cr {
				s.csi(s.extent-1-s.cr, 'B')
				s.cr = s.extent - 1
			}
			for i := s.cr; i < r; i++ {
				s.buf.WriteString("\r\n")
			}
			s.extent = r + 1
			s.cc = 0
		}
	}
	if r != s.cr || c != s.cc {
		switch {
		case c == 0:
			s.buf.WriteByte('\r')
		case r == s.cr && c > s.cc:
			s.csi(c-s.cc, 'C')
		case r == s.cr && c < s.cc && s.cc-c < c:
			s.csi(s.cc-c, 'D')
		default:
			s.buf.WriteByte('\r')
			s.csi(c, 'C')
		}
	}
	s.cr, s.cc = r, c
}

// render draws rows with the cursor at (cr, cc), sending only the difference
// from what is on screen.
func (s *screen) render(rows []string, cr, cc int) {
	s.draw(rows, cr, cc)
	s.flush()
}

func (s *screen) draw(rows []string, cr, cc int) {
	cr = max(0, min(cr, len(rows)-1))
	if !s.drawn {
		// Nothing of ours is on screen: the cursor is where the region's top
		// row belongs, at column 0 of a line of its own.
		s.cr, s.cc, s.extent = 0, 0, 1
		s.buf.WriteString("\r")
		for i, row := range rows {
			if i > 0 {
				s.buf.WriteString("\r\n")
				s.extent++
			}
			s.buf.WriteString(row)
			s.buf.WriteString("\x1b[0m\x1b[K")
			s.cr, s.cc = i, displayWidth(row)
		}
		s.buf.WriteString("\x1b[J")
		s.drawn = true
		s.rows = append(s.rows[:0], rows...)
		s.moveTo(cr, cc)
		return
	}

	n := max(len(rows), len(s.rows))
	for i := 0; i < n; i++ {
		switch {
		case i < len(rows) && i < len(s.rows):
			if rows[i] != s.rows[i] {
				s.rowDiff(i, s.rows[i], rows[i])
			}
		case i < len(rows):
			s.moveTo(i, 0)
			s.buf.WriteString(rows[i])
			s.buf.WriteString("\x1b[0m\x1b[K")
			s.cc = displayWidth(rows[i])
		default:
			// The region got shorter: clear from the first row it no longer
			// has to the end of the screen.
			if len(rows) == 0 {
				s.moveTo(0, 0)
			} else {
				s.moveTo(len(rows)-1, displayWidth(rows[len(rows)-1]))
				s.moveTo(len(rows), 0)
			}
			s.buf.WriteString("\x1b[J")
			i = n
		}
	}
	s.rows = append(s.rows[:0], rows...)
	s.moveTo(cr, cc)
}

// rowDiff rewrites row i from the first cell where old and new differ. When
// the rows then agree again from some point to the end, and the changed
// stretch keeps its width, only that stretch is written: a spinner's glyph,
// or a counter, without the rest of its row.
func (s *screen) rowDiff(i int, old, new string) {
	ot := tokens(old)
	nt := tokens(new)
	k := 0
	for k < len(ot) && k < len(nt) && ot[k] == nt[k] {
		k++
	}
	// The common suffix, which must not overlap the common prefix.
	e := 0
	for e < len(ot)-k && e < len(nt)-k && ot[len(ot)-1-e] == nt[len(nt)-1-e] {
		e++
	}
	middle := nt[k : len(nt)-e]
	// A changed escape restyles what follows it, so then the rest is written.
	stopAtSuffix := e > 0 && widthOf(middle) == widthOf(ot[k:len(ot)-e]) &&
		!hasEsc(middle) && !hasEsc(ot[k:len(ot)-e])

	// The column and the styling in force at the first difference.
	col := 0
	var sgr []string
	for _, t := range nt[:k] {
		if t.esc {
			if isSGR(t.s) {
				if isReset(t.s) {
					sgr = sgr[:0]
				} else {
					sgr = append(sgr, t.s)
				}
			}
			continue
		}
		col += t.w
	}
	s.moveTo(i, col)
	for _, e := range sgr {
		s.buf.WriteString(e)
	}
	write := nt[k:]
	if stopAtSuffix {
		write = middle
	}
	w := col
	styled := len(sgr) > 0
	for _, t := range write {
		s.buf.WriteString(t.s)
		w += t.w
		if t.esc && isSGR(t.s) {
			styled = true
		}
	}
	if styled {
		s.buf.WriteString("\x1b[0m")
	}
	if !stopAtSuffix && w < widthOf(ot) {
		s.buf.WriteString("\x1b[K")
	}
	s.cc = w
}

type token struct {
	s   string
	esc bool
	w   int
}

func tokens(s string) []token {
	var out []token
	forEachToken(s, func(t string, esc bool) {
		w := 0
		if !esc {
			w = clusterWidth([]rune(t))
		}
		out = append(out, token{t, esc, w})
	})
	return out
}

func hasEsc(ts []token) bool {
	for _, t := range ts {
		if t.esc {
			return true
		}
	}
	return false
}

func widthOf(ts []token) int {
	n := 0
	for _, t := range ts {
		n += t.w
	}
	return n
}

// commit writes lines above the region as finished transcript, then draws
// the region again beneath them. Committed lines scroll away with the
// terminal and are never touched again.
func (s *screen) commit(lines []string, rows []string, cr, cc int) {
	s.buf.WriteString("\x1b[?2026h")
	if s.drawn {
		s.moveTo(0, 0)
		s.buf.WriteString("\x1b[J")
	} else {
		s.buf.WriteString("\r")
	}
	for _, l := range lines {
		s.buf.WriteString(l)
		s.buf.WriteString("\x1b[0m\r\n")
	}
	s.drawn = false
	s.rows = s.rows[:0]
	s.cr, s.cc, s.extent = 0, 0, 0
	s.draw(rows, cr, cc)
	s.buf.WriteString("\x1b[?2026l")
	s.flush()
}

// clear erases the region and leaves the cursor at its top, at column 0, for
// output that is not ours (an external editor, the program exiting).
func (s *screen) clear() {
	if s.drawn {
		s.moveTo(0, 0)
		s.buf.WriteString("\x1b[J")
	}
	s.drawn = false
	s.rows = s.rows[:0]
	s.cr, s.cc, s.extent = 0, 0, 0
	s.flush()
}

// forget drops what the screen was showing, after something else cleared it
// (a resize repaint, Ctrl-L): the next render starts from the cursor.
func (s *screen) forget() {
	s.drawn = false
	s.rows = s.rows[:0]
	s.cr, s.cc, s.extent = 0, 0, 0
}

// raw sends bytes that are not part of a frame, such as a mode switch.
func (s *screen) raw(str string) {
	s.buf.WriteString(str)
	s.flush()
}
