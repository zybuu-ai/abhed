package ui

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// approvalGuard is the quiet a dialog's answer needs: after the dialog
// appears, after the key before it, and after itself.
const approvalGuard = 300 * time.Millisecond

// dialogState is a dialog on screen.
type dialogState struct {
	spec     DialogSpec
	sel      int
	shownAt  time.Time // when it was first drawn; zero until then
	pending  int       // a number key waiting to stand alone, or -1
	pendingN int
	// before is the selection a pending number replaced, put back if it fails;
	// byArrow says an arrow, after the guard, made the selection.
	before  int
	byArrow bool
	note    string
	done    chan int
}

func (s *dialogState) index(id string) int {
	for i, c := range s.spec.Choices {
		if c.ID == id {
			return i
		}
	}
	return -1
}

func (s *dialogState) cancelIndex() int {
	if i := s.index(s.spec.Cancel); i >= 0 {
		return i
	}
	if i := s.index("no"); i >= 0 {
		return i
	}
	return len(s.spec.Choices) - 1
}

// ask shows spec and waits for an answer, the context's end, or the end of
// input. One dialog is on screen at a time; others wait their turn.
func (d *dock) ask(ctx context.Context, spec DialogSpec) (string, error) {
	if len(spec.Choices) == 0 {
		return "", fmt.Errorf("dialog %q has no choices", spec.Title)
	}
	select {
	case d.askSlot <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-d.askSlot }()

	// Nothing is selected unless the dialog has a default: Enter alone then
	// answers nothing, so an approval is always a choice someone made.
	st := &dialogState{spec: spec, pending: -1, done: make(chan int, 1)}
	st.sel = st.index(spec.Default)
	d.mu.Lock()
	if d.inputEnded {
		d.mu.Unlock()
		return spec.Choices[st.cancelIndex()].ID, io.EOF
	}
	d.dlg = st
	d.next = "" // no offered prompt beside a question
	d.scr.raw("\x1b[?25l")
	d.draw()
	// The guard runs from the moment the choices reach the screen — not
	// while the full-screen view or the editor hides them; those start it
	// when they give the screen back.
	if !d.stopped && d.pager == nil {
		st.shownAt = d.now()
	}
	d.mu.Unlock()

	var i int
	var err error
	select {
	case i = <-st.done:
	case <-ctx.Done():
		i, err = -1, ctx.Err()
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dlg == st {
		d.dlg = nil
	}
	d.scr.raw("\x1b[?25h")
	if i < 0 {
		d.draw()
		return "", err
	}
	id := spec.Choices[i].ID
	if !spec.NoRecord {
		d.commit(&dialogRecord{spec: spec, chosen: i})
	} else {
		d.draw()
	}
	return id, nil
}

// endDialogs answers any open dialog with its cancel choice, as input ends.
func (d *dock) endDialogs() {
	d.inputEnded = true
	if d.dlg != nil {
		d.resolve(d.dlg.cancelIndex())
	}
}

func (d *dock) resolve(i int) {
	st := d.dlg
	if st == nil {
		return
	}
	d.dlg = nil
	select {
	case st.done <- i:
	default:
	}
}

// dialogKey applies a key to the dialog on screen.
//
// Nothing counts for approvalGuard after the dialog shows; then a number must
// stand alone, no letter moves, and Enter never approves (see 20-terminal.md).
//
// Ctrl-C is the one exception: it declines at once, which is always safe.
func (d *dock) dialogKey(k key, at time.Time, gap time.Duration) {
	st := d.dlg
	if k.code == kNone && k.r == keyCtrlC {
		d.resolve(st.cancelIndex())
		return
	}
	if st.shownAt.IsZero() || at.Sub(st.shownAt) < approvalGuard {
		return
	}
	if st.pending >= 0 {
		// Another key followed a number before it stood alone: typing. The
		// number chose nothing, so it leaves nothing selected for Enter.
		st.pending, st.sel, st.byArrow = -1, st.before, false
		st.note = "keys pressed together are ignored; press one number"
		return
	}
	quiet := gap >= approvalGuard
	switch {
	case k.code == kEsc:
		d.resolve(st.cancelIndex())
	// No letter moves the selection: typed text must never choose.
	case k.code == kUp || k.code == kNone && k.r == keyCtrlP:
		if st.sel < 0 {
			st.sel = len(st.spec.Choices)
		}
		st.sel = (st.sel + len(st.spec.Choices) - 1) % len(st.spec.Choices)
		st.byArrow, st.note = true, ""
	case k.code == kDown || k.code == kNone && k.r == keyCtrlN:
		st.sel = (st.sel + 1) % len(st.spec.Choices)
		st.byArrow, st.note = true, ""
	case k.code == kNone && k.r == keyCtrlO:
		d.openDialogPager(st)
	case k.code == kNone && k.r == keyEnter:
		// An approval or a confirm is answered by its number; Enter may only
		// decline. A choice takes Enter on what an arrow selected.
		strict := st.spec.Kind == DialogApproval || st.spec.Kind == DialogConfirm
		switch {
		case st.sel < 0 || strict && st.sel != st.cancelIndex():
			st.note = "press the number of your answer"
			return
		case !strict && !st.byArrow:
			st.note = "choose with a number, or ↑↓ then Enter"
			return
		case !quiet:
			// An arrow is a key, so Enter straight after one is not quiet.
			st.note = "too quick after another key; press Enter again"
			return
		}
		d.resolve(st.sel)
	case k.code == kNone && !k.alt:
		i := st.choiceFor(k.r)
		if i < 0 {
			return // only the choices offered can be chosen
		}
		if !quiet {
			st.note = "too quick after another key; press it again"
			return
		}
		st.before = st.sel
		st.sel = i
		st.pending = i
		st.pendingN++
		n := st.pendingN
		d.after(approvalGuard, func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.dlg != st || st.pendingN != n || st.pending < 0 {
				return
			}
			d.resolve(st.pending)
			d.draw()
		})
	}
}

// choiceFor maps a number to its choice's index, or -1: no letter answers.
func (s *dialogState) choiceFor(r rune) int {
	if r >= '1' && r <= '9' {
		if i := int(r - '1'); i < len(s.spec.Choices) {
			return i
		}
	}
	return -1
}

func (d *dock) openDialogPager(st *dialogState) {
	var rows []string
	for _, b := range st.spec.Body {
		rows = append(rows, blockView(b).lines(d.contentWidth(), d.st, true)...)
	}
	p := &pager{rows: rows}
	d.pager = p
	d.scr.raw("\x1b[?1049h")
	d.drawPager()
}

// rows draws the dialog in at most maxRows rows. The choices are always
// shown whole, wrapped rather than cut, so what "always" would allow is
// never hidden; the body gives way first.
func (st *dialogState) rows(d *dock, w, maxRows int) []string {
	s := d.st
	bar := s.Accent("│ ")
	inner := max(w-2, 10)
	var head, body, tail []string

	for i, l := range wrapWords(sanitize(st.spec.Title, true), w-4) {
		lead := s.Accent("╭─ ")
		if i > 0 {
			lead = bar + " "
		}
		head = append(head, lead+s.Bold(l))
	}
	for _, l := range whyRows(sanitize(st.spec.Why, true), inner) {
		head = append(head, bar+s.Dim(l))
	}
	for _, b := range st.spec.Body {
		for _, l := range blockView(b).lines(inner, s, false) {
			body = append(body, bar+l)
		}
	}

	if st.spec.Ask != "" {
		for _, l := range wrapWords(sanitize(st.spec.Ask, true), inner) {
			tail = append(tail, bar+s.Bold(l))
		}
	}
	// A long list shows a window of choices around the selected one.
	first, last := 0, len(st.spec.Choices)
	if window := max(maxRows-len(head)-4, 3); last > window {
		first = min(max(0, max(st.sel, 0)-window/2), last-window)
		last = first + window
	}
	if first > 0 {
		tail = append(tail, bar+s.Dim(fmt.Sprintf("  ↑ %d more", first)))
	}
	for i := first; i < last; i++ {
		c := st.spec.Choices[i]
		num := strconv.Itoa(i+1) + ". "
		label := sanitize(c.Label, true)
		mark := "  "
		if i == st.sel {
			mark = s.Accent("❯ ")
		}
		lines := wrapWords(label, max(inner-2-len(num), 8))
		for j, l := range lines {
			lead := mark + num
			if j > 0 {
				lead = "  " + strings.Repeat(" ", len(num))
			}
			switch {
			case i == st.sel:
				l = s.Accent(l)
			case c.Destructive:
				l = s.Red(l)
			}
			tail = append(tail, bar+lead+l)
		}
	}
	if last < len(st.spec.Choices) {
		tail = append(tail, bar+s.Dim(fmt.Sprintf("  ↓ %d more", len(st.spec.Choices)-last)))
	}
	hint := "number or ↑↓ then enter · esc to decline"
	if st.note != "" {
		hint = st.note
	}
	tail = append(tail, s.Accent("╰─ ")+s.Dim(truncateWidth(hint, w-4)))

	room := maxRows - len(head) - len(tail)
	if room < len(body) {
		keep := max(room-1, 0)
		more := len(body) - keep
		body = append(body[:keep], bar+s.Dim(fmt.Sprintf("… %d more rows · ctrl+o shows all", more)))
		if keep == 0 && room <= 0 {
			body = nil
		}
	}
	return append(append(head, body...), tail...)
}

// dialogRecord is what an answered dialog leaves in the transcript: its
// title, its body and the answer, so the record on screen matches the one on
// disk.
type dialogRecord struct {
	spec   DialogSpec
	chosen int
}

func (r *dialogRecord) lines(width int, s Style, expanded bool) []string {
	c := r.spec.Choices[r.chosen]
	var out []string
	for i, l := range wrapWords(sanitize(r.spec.Title, true), width-2) {
		lead := s.Accent("● ")
		if i > 0 {
			lead = "  "
		}
		out = append(out, lead+s.Bold(l))
	}
	for _, l := range whyRows(sanitize(r.spec.Why, true), width-2) {
		out = append(out, "  "+s.Dim(l))
	}
	for _, b := range r.spec.Body {
		for _, l := range blockView(b).lines(width-2, s, expanded) {
			out = append(out, "  "+l)
		}
	}
	outcome := c.Label
	if r.spec.Outcome != nil {
		outcome = r.spec.Outcome(c.ID)
	}
	mark := s.Green("✓ ")
	if c.ID == "no" || c.ID == r.spec.Cancel {
		mark = s.Red("✕ ")
	}
	for i, l := range wrapWords(sanitize(outcome, true), width-4) {
		lead := "  ⎿ " + mark
		if i > 0 {
			lead = "      "
		}
		out = append(out, lead+l)
	}
	return out
}

// whyRows wraps the explanation, line by line.
func whyRows(why string, width int) []string {
	var out []string
	for _, line := range strings.Split(why, "\n") {
		for _, l := range wrapWords(line, width) {
			if strings.TrimSpace(l) != "" {
				out = append(out, l)
			}
		}
	}
	return out
}
