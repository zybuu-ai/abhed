package ui

import (
	"context"
	"errors"
	"io"
)

// The terminal is a Surface: blocks join the transcript, questions are the
// dock's guarded dialog, a pick is a dialog of the items, a panel is the
// full-screen view Ctrl-O uses.
var _ Surface = (*LineReader)(nil)

// NewSurface is the session's Surface: the terminal when there is one, and
// otherwise a LineSurface answered a line at a time from in.
func NewSurface(l *LineReader, in LineAnswers) Surface {
	if l != nil && l.raw {
		return l
	}
	return NewLineSurface(LazyStdout{}, NewStyle(LazyStdout{}), in)
}

// Append adds b to the transcript.
func (l *LineReader) Append(b Block) {
	if !l.raw {
		return
	}
	l.commitItem(blockView(b))
}

// Dialog shows a guarded numbered dialog in the dock and returns the chosen
// choice's ID, or ErrNoAnswer when nobody chose: input ended, or ctx was
// cancelled (whose error is wrapped).
func (l *LineReader) Dialog(ctx context.Context, spec DialogSpec) (string, error) {
	if !l.raw {
		return "", ErrNoAnswer
	}
	spec, err := spec.Normalized()
	if err != nil {
		return "", err
	}
	id, err := l.d.ask(ctx, spec)
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, io.EOF):
		return "", ErrNoAnswer
	case ctx.Err() != nil:
		return "", errors.Join(ErrNoAnswer, ctx.Err())
	}
	return "", err
}

// Pick asks for one of p's items, as a dialog of them: arrows and Enter,
// or an item's number.
func (l *LineReader) Pick(ctx context.Context, p PickSpec) (string, error) {
	if len(p.Items) == 0 {
		return "", ErrNoAnswer
	}
	def := p.Default
	if def == "" {
		def = p.Items[0].ID // picking is not a grant: Enter may take the first
	}
	spec := DialogSpec{Kind: DialogChoice, Title: p.Title, Default: def, NoRecord: true, Cancel: "\x00cancel",
		Filter: p.Filter, Pinned: []string{"\x00cancel"}}
	for _, it := range p.Items {
		label := sanitize(it.Label, false)
		if it.Detail != "" {
			label += "  " + l.d.st.Dim(sanitize(it.Detail, false))
		}
		spec.Choices = append(spec.Choices, Choice{ID: it.ID, Label: label})
		if it.Always {
			spec.Pinned = append(spec.Pinned, it.ID)
		}
	}
	spec.Choices = append(spec.Choices, Choice{ID: "\x00cancel", Label: "Cancel (esc)"})
	id, err := l.Dialog(ctx, spec)
	if err != nil {
		return "", err
	}
	if id == "\x00cancel" {
		return "", ErrNoAnswer
	}
	return id, nil
}

// Panel shows p in the full-screen view and returns when it is closed.
func (l *LineReader) Panel(ctx context.Context, p PanelSpec) error {
	if !l.raw {
		return ErrNoAnswer
	}
	d := l.d
	d.mu.Lock()
	d.measure()
	var rows []string
	for _, b := range p.Body {
		rows = append(rows, blockView(b).lines(d.contentWidth(), d.st, true)...)
	}
	done := make(chan struct{})
	if d.pager != nil {
		d.closePager()
	}
	d.pager = &pager{rows: rows, title: p.Title, done: done, shownAt: d.now()}
	d.scr.raw("\x1b[?1049h\x1b[?25l")
	d.drawPager()
	d.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		d.mu.Lock()
		if d.pager != nil && d.pager.done == done {
			d.closePager()
		}
		d.mu.Unlock()
		return ctx.Err()
	}
}

// SetStatus replaces what the footer shows, unless SetStatusFunc supplies it.
func (l *LineReader) SetStatus(m StatusModel) {
	if !l.raw {
		return
	}
	l.d.mu.Lock()
	l.d.statusSet = m
	l.d.draw()
	l.d.mu.Unlock()
}

// Notify shows t in the footer for a moment.
func (l *LineReader) Notify(t Toast) {
	if !l.raw {
		return
	}
	l.d.mu.Lock()
	l.d.flash(sanitize(t.Text, false))
	l.d.draw()
	l.d.mu.Unlock()
}

// Send hands line to the session as if it had been typed and submitted,
// without drawing it or keeping it in the history: how a key the session
// acts on reaches it, on the session's goroutine.
func (l *LineReader) Send(line string) {
	if !l.raw {
		return
	}
	l.d.mu.Lock()
	l.d.push(readResult{line, nil})
	l.d.mu.Unlock()
}
