package ui

import (
	"fmt"
	"strings"
)

// pager is the transcript view Ctrl-O opens: the whole session with every
// tool's output and every reasoning block in full, on the terminal's
// alternate screen so closing it puts the session back exactly as it was.
type pager struct {
	rows  []string
	top   int
	title string
	// done is closed when the view is closed, for a Panel waiting on it.
	done chan struct{}
}

func (d *dock) openPager() {
	d.measure()
	var rows []string
	for _, b := range d.tr.snapshot() {
		rows = append(rows, b.lines(d.contentWidth(), d.st, true)...)
	}
	if len(rows) == 0 {
		rows = []string{d.st.Dim("  nothing in the transcript yet")}
	}
	p := &pager{rows: rows}
	p.top = max(0, len(rows)-d.pageRows())
	d.pager = p
	d.scr.raw("\x1b[?1049h\x1b[?25l")
	d.drawPager()
}

func (d *dock) closePager() {
	if d.pager.done != nil {
		close(d.pager.done)
	}
	d.pager = nil
	d.scr.raw("\x1b[?25h\x1b[?1049l")
	if d.dlg != nil {
		d.scr.raw("\x1b[?25l")
	}
	// The main screen comes back as it was, region and all; what was
	// committed meanwhile is drawn now.
	missed := d.missed
	d.missed = nil
	var lines []string
	for _, b := range missed {
		lines = append(lines, b.lines(d.contentWidth(), d.st, false)...)
	}
	rows, cr, cc := d.frame()
	d.scr.commit(lines, rows, cr, cc)
}

func (d *dock) pageRows() int { return max(1, d.height-2) }

func (d *dock) drawPager() {
	p := d.pager
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	end := min(len(p.rows), p.top+d.pageRows())
	name := "Transcript"
	if p.title != "" {
		name = p.title
	}
	title := fmt.Sprintf(" %s · %d–%d of %d ", name, p.top+1, end, len(p.rows))
	b.WriteString(d.st.Reverse(truncateWidth(sanitize(title, false), d.contentWidth())) + "\r\n")
	for _, r := range p.rows[p.top:end] {
		b.WriteString(truncateWidth(sanitize(r, true), d.contentWidth()) + "\x1b[0m\r\n")
	}
	b.WriteString(d.st.Dim(truncateWidth("↑↓ pgup pgdn g G to move · q, esc or ctrl+o to close", d.contentWidth())))
	d.scr.raw(b.String())
}

func (d *dock) pagerKey(k key) {
	p := d.pager
	page := d.pageRows()
	last := max(0, len(p.rows)-page)
	switch {
	case k.code == kUp || k.r == 'k' && k.code == kNone:
		p.top--
	case k.code == kDown || k.r == 'j' && k.code == kNone:
		p.top++
	case k.code == kPgUp || k.r == 'b' && k.code == kNone:
		p.top -= page
	case k.code == kPgDn || k.r == ' ' && k.code == kNone:
		p.top += page
	case k.code == kHome || k.r == 'g' && k.code == kNone:
		p.top = 0
	case k.code == kEnd || k.r == 'G' && k.code == kNone:
		p.top = last
	case k.code == kEsc || k.code == kNone && (k.r == 'q' || k.r == keyCtrlO || k.r == keyCtrlC):
		d.closePager()
		return
	default:
		return
	}
	p.top = max(0, min(p.top, last))
	d.drawPager()
}
