package ui

import (
	"fmt"
	"strings"
	"time"
)

// WorkState is how a row of the work panel stands.
type WorkState int

const (
	WorkIdle WorkState = iota
	WorkRunning
	WorkDone
	WorkFailed
	WorkCancelled
)

// WorkRow is one row of the work panel under the input: the conversation
// itself first (ID ""), then its subagents and background jobs.
type WorkRow struct {
	ID string
	// Depth is 0 for the conversation, 1 for what it started, 2 and more
	// for what those started.
	Depth    int
	Kind     string // the agent type, or the job's kind
	Title    string
	Activity string
	State    WorkState
	Elapsed  time.Duration
	Tokens   int
	// Target is set when a message typed while the row is selected goes to it.
	Target bool
}

// workVisible is how many rows besides the conversation the panel shows.
const workVisible = 5

// SetWork connects the work panel: rows is asked on every draw, from the
// key reader's goroutine; open shows a row's record; send hands a message
// to a row's agent and reports whether it took it.
func (l *LineReader) SetWork(rows func() []WorkRow, open func(id string), send func(id, text string) bool) {
	if !l.raw {
		return
	}
	l.d.mu.Lock()
	l.d.work, l.d.workOpen, l.d.workSend = rows, open, send
	l.d.draw()
	l.d.mu.Unlock()
}

// Selected is the row of the work panel selected now, "" for the conversation.
func (l *LineReader) Selected() string {
	if !l.raw {
		return ""
	}
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	return l.d.workSel
}

// workRows is the panel's rows now; none unless something besides the
// conversation is listed. The selection follows them.
func (d *dock) workRows() []WorkRow {
	if d.work == nil {
		return nil
	}
	rows := d.work()
	if len(rows) < 2 {
		d.workSel = ""
		return nil
	}
	if d.workSel != "" && workIndex(rows, d.workSel) < 0 {
		d.workSel = ""
	}
	return rows
}

func workIndex(rows []WorkRow, id string) int {
	for i, r := range rows {
		if r.ID == id {
			return i
		}
	}
	return -1
}

// workNav reports whether the arrows move the panel's selection: only with
// nothing typed and nothing else open that takes them.
func (d *dock) workNav() bool {
	return d.buf.empty() && len(d.menu) == 0 && !d.searching && d.dlg == nil && d.vimInsert() && len(d.workRows()) > 0
}

// moveWork moves the selection; Up from the conversation is left to history.
func (d *dock) moveWork(delta int) bool {
	rows := d.workRows()
	i := max(workIndex(rows, d.workSel), 0)
	if delta < 0 && i == 0 {
		return false
	}
	i = min(max(i+delta, 0), len(rows)-1)
	d.workSel = rows[i].ID
	return true
}

// selectedRow is the selected row, if one besides the conversation is.
func (d *dock) selectedRow() (WorkRow, bool) {
	if d.workSel == "" {
		return WorkRow{}, false
	}
	rows := d.workRows()
	if i := workIndex(rows, d.workSel); i > 0 {
		return rows[i], true
	}
	return WorkRow{}, false
}

// placeholder is what the empty input says: who a message goes to.
func (d *dock) placeholder() string {
	r, ok := d.selectedRow()
	if !ok {
		return ""
	}
	name := oneLine(orStr(r.Kind, "task"))
	if r.Target {
		return "Message @" + name + "…"
	}
	return name + " can't take messages; a message goes to main"
}

// workPanel draws the panel: a hint, the conversation, and up to
// workVisible rows around the selection.
func workPanel(s Style, rows []WorkRow, sel string, w int) []string {
	if len(rows) < 2 {
		return nil
	}
	hint := "↑/↓ to select · Enter to view"
	if sel != "" {
		hint += " · Esc to go back to main"
	}
	out := []string{"  " + s.Dim(truncateWidth(hint, w-2)), ""}
	out = append(out, workLine(s, rows[0], sel == "", 0, w))
	rest := rows[1:]
	i := workIndex(rest, sel)
	start := 0
	if i >= workVisible {
		start = i - workVisible + 1
	}
	end := min(start+workVisible, len(rest))
	kindW := 0
	for _, r := range rest[start:end] {
		kindW = max(kindW, displayWidth(oneLine(orStr(r.Kind, "task"))))
	}
	kindW = min(kindW, max(8, w/4))
	if start > 0 {
		out = append(out, "  "+s.Dim(fmt.Sprintf("↑ %d more", start)))
	}
	for _, r := range rest[start:end] {
		out = append(out, workLine(s, r, r.ID == sel, kindW, w))
	}
	if more := len(rest) - end; more > 0 {
		out = append(out, "  "+s.Dim(fmt.Sprintf("↓ %d more", more)))
	}
	return out
}

// workMark is a row's state as a mark that reads without colour.
func workMark(s Style, st WorkState) string {
	switch st {
	case WorkRunning:
		return s.Accent("●")
	case WorkDone:
		return s.Green("✓")
	case WorkFailed:
		return s.Red("✕")
	}
	return s.Dim("○")
}

// workLine is one row: selection, nesting, mark, kind, title and activity
// on the left; elapsed time and tokens on the right, dropped first when
// the row is narrow.
func workLine(s Style, r WorkRow, selected bool, kindW, w int) string {
	lead := "  "
	if selected {
		lead = s.Accent("❯") + " "
	}
	if r.Depth > 1 {
		lead += strings.Repeat("  ", r.Depth-2) + s.Dim("└") + " "
	}
	name := "main"
	var detail string
	if r.ID != "" {
		name = padRight(truncateWidth(oneLine(orStr(r.Kind, "task")), kindW), kindW)
		detail = oneLine(r.Title)
		if a := oneLine(r.Activity); a != "" {
			detail += s.Dim(" · " + a)
		}
	}
	if selected {
		name = s.Bold(name)
	}
	left := lead + workMark(s, r.State) + " " + name
	if detail != "" {
		left += "  " + detail
	}
	var meta []string
	if r.Elapsed > 0 {
		meta = append(meta, formatElapsed(r.Elapsed))
	}
	if r.Tokens > 0 {
		meta = append(meta, formatCount(r.Tokens)+" tokens in")
	}
	right := s.Dim(strings.Join(meta, " · "))
	rw := displayWidth(right)
	if rw == 0 || w-rw-2 < displayWidth(stripANSI(lead))+16 {
		return truncateWidth(left, w)
	}
	left = truncateWidth(left, w-rw-2)
	return left + strings.Repeat(" ", w-displayWidth(left)-rw) + right
}

// oneLine is text from a model or a command as one row: its line breaks and
// runs of space as single spaces, and hidden characters as escapes, as /tasks
// writes them; dropping them made the two disagree.
func oneLine(s string) string { return VisibleLine(strings.Join(strings.Fields(s), " ")) }
