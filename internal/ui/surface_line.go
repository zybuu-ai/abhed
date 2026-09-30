package ui

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// LineAnswers is where a LineSurface reads answers: one typed line at a time.
// The Prompter is one, so a dialog shares the session's single stdin reader.
// ok is false when input ended or the context was cancelled.
type LineAnswers interface {
	Await(ctx context.Context) (line string, ok bool)
}

// lineAttempts is how many unmatched answers a question takes before it
// gives up as unanswered, so a script feeding the wrong lines cannot loop.
const lineAttempts = 3

// LineSurface is the Surface for piped input and terminals without cursor
// control: blocks print as lines, a dialog is a numbered question answered
// by a line. It is guarded like the terminal one: only a typed answer picks a
// choice, an empty one takes only a safe default, a destructive choice needs
// a second "yes", and input ending is no answer, never a yes.
type LineSurface struct {
	out io.Writer
	s   Style
	in  LineAnswers

	mu     sync.Mutex
	status StatusModel
}

var _ Surface = (*LineSurface)(nil)

// NewLineSurface draws on out in style s and reads answers from in; with a
// nil in, every question is unanswered.
func NewLineSurface(out io.Writer, s Style, in LineAnswers) *LineSurface {
	return &LineSurface{out: out, s: s, in: in}
}

func (l *LineSurface) printf(format string, args ...any) { fmt.Fprintf(l.out, format, args...) }

// Append prints a block.
func (l *LineSurface) Append(b Block) {
	switch b.Kind {
	case BlockError:
		l.printf("  %s %s\n", l.s.Red("✕"), plainText(b.Text))
	case BlockNotice:
		l.printf("  %s\n", l.s.Dim(plainText(b.Text)))
	case BlockTable:
		l.table(b.Rows)
	case BlockDiff:
		if b.Path != "" {
			l.printf("  %s\n", l.s.Bold(plainText(b.Path)))
		}
		for _, line := range strings.Split(strings.TrimRight(plainText(b.Text), "\n"), "\n") {
			switch {
			case strings.HasPrefix(line, "+"):
				line = l.s.Green(line)
			case strings.HasPrefix(line, "-"):
				line = l.s.Red(line)
			}
			l.printf("    %s\n", line)
		}
	default: // markdown and tool output print as text
		if b.Path != "" {
			l.printf("  %s\n", l.s.Bold(plainText(b.Path)))
		}
		for _, line := range strings.Split(strings.TrimRight(plainText(b.Text), "\n"), "\n") {
			l.printf("  %s\n", line)
		}
	}
}

func (l *LineSurface) table(rows [][]string) {
	var widths []int
	for _, r := range rows {
		for i, c := range r {
			if i >= len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], len([]rune(plainText(c))))
		}
	}
	for n, r := range rows {
		var b strings.Builder
		for i, c := range r {
			cell := plainText(c)
			b.WriteString(cell)
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-len([]rune(cell))+2))
			}
		}
		line := b.String()
		if n == 0 {
			line = l.s.Bold(line)
		}
		l.printf("  %s\n", line)
	}
}

// Dialog prints the question and its numbered choices and reads the answer.
func (l *LineSurface) Dialog(ctx context.Context, spec DialogSpec) (string, error) {
	d, err := spec.Normalized()
	if err != nil {
		return "", err
	}
	if d.Title != "" {
		l.printf("  %s\n", l.s.Bold(plainText(d.Title)))
	}
	for _, b := range d.Body {
		l.Append(b)
	}
	if d.Why != "" {
		l.printf("  %s\n", l.s.Dim(plainText(d.Why)))
	}
	for i, c := range d.Choices {
		l.printf("  %d. %s\n", i+1, plainText(c.Label))
	}
	for range lineAttempts {
		if def, ok := d.choice(d.Default); ok {
			l.printf("  answer 1-%d, or enter for %s: ", len(d.Choices), plainText(def.Label))
		} else {
			l.printf("  answer 1-%d: ", len(d.Choices))
		}
		answer, ok := l.read(ctx)
		if !ok {
			l.printf("\n")
			return "", ErrNoAnswer
		}
		if answer == "" {
			if d.Default != "" {
				return d.Default, nil
			}
			continue
		}
		c, found := d.match(answer)
		if !found {
			l.printf("  %s\n", l.s.Dim(fmt.Sprintf("%q is not one of the choices", plainText(answer))))
			continue
		}
		if !c.Destructive {
			return c.ID, nil
		}
		l.printf("  %s cannot be undone; type yes to confirm: ", plainText(c.Label))
		confirm, ok := l.read(ctx)
		if ok && strings.EqualFold(confirm, "yes") {
			return c.ID, nil
		}
		if d.Default != "" {
			return d.Default, nil
		}
		return "", ErrNoAnswer
	}
	return "", ErrNoAnswer
}

// Pick prints the numbered items and reads which one.
func (l *LineSurface) Pick(ctx context.Context, p PickSpec) (string, error) {
	if len(p.Items) == 0 {
		return "", ErrNoAnswer
	}
	if p.Title != "" {
		l.printf("  %s\n", l.s.Bold(plainText(p.Title)))
	}
	for i, it := range p.Items {
		line := fmt.Sprintf("  %d. %s", i+1, plainText(it.Label))
		if it.Detail != "" {
			line += "  " + l.s.Dim(plainText(it.Detail))
		}
		l.printf("%s\n", line)
	}
	for range lineAttempts {
		l.printf("  choose 1-%d: ", len(p.Items))
		answer, ok := l.read(ctx)
		if !ok {
			l.printf("\n")
			return "", ErrNoAnswer
		}
		if answer == "" && p.Default != "" {
			return p.Default, nil
		}
		if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(p.Items) {
			return p.Items[n-1].ID, nil
		}
		for _, it := range p.Items {
			if answer != "" && answer == it.ID {
				return it.ID, nil
			}
		}
	}
	return "", ErrNoAnswer
}

// Panel prints the view; there is nothing to close.
func (l *LineSurface) Panel(_ context.Context, p PanelSpec) error {
	if p.Title != "" {
		l.printf("  %s\n", l.s.Bold(plainText(p.Title)))
	}
	for _, b := range p.Body {
		l.Append(b)
	}
	return nil
}

// SetStatus keeps the model for Status; a line surface has no footer.
func (l *LineSurface) SetStatus(m StatusModel) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.status = m
}

// Status is the last model set.
func (l *LineSurface) Status() StatusModel {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status
}

// Notify prints the toast as a dim line.
func (l *LineSurface) Notify(t Toast) {
	if t.Warn {
		l.printf("  %s %s\n", l.s.Yellow("!"), plainText(t.Text))
		return
	}
	l.printf("  %s\n", l.s.Dim(plainText(t.Text)))
}

func (l *LineSurface) read(ctx context.Context) (string, bool) {
	if l.in == nil || ctx.Err() != nil {
		return "", false
	}
	line, ok := l.in.Await(ctx)
	if !ok || ctx.Err() != nil {
		return "", false
	}
	return strings.TrimSpace(line), true
}

// plainText drops control characters, escape sequences' introducer included,
// so text from a model or a file cannot move the cursor, set the title or
// write the clipboard. Newlines and tabs stay.
func plainText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
