package ui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func (d *dock) statusModel() StatusModel {
	if d.status == nil {
		return d.statusSet
	}
	m := d.status()
	if m.Line == "" {
		m.Line = d.statusSet.Line
	}
	return m
}

// footerRows are the two rows under the input: the permission mode, always
// visible, with a hint; and the status line.
func (d *dock) footerRows(w int) []string {
	s := d.st
	m := d.statusModel()
	// What the session reports comes from providers, git and a command:
	// only text is drawn, and a status line's colours only where colour is on.
	m.Model, m.Provider = sanitize(m.Model, false), sanitize(m.Provider, false)
	m.GitBranch, m.Cwd = sanitize(m.GitBranch, false), sanitize(m.Cwd, false)
	m.SessionTitle = sanitize(m.SessionTitle, false)
	m.Mode, m.PendingMode = sanitize(m.Mode, false), sanitize(m.PendingMode, false)
	m.Line = sanitize(m.Line, s.enabled)

	left := modeLabel(s, m)
	right := ""
	switch {
	case d.hint != "":
		right = d.hint
	case d.searching:
		right = "ctrl+r older · enter to edit · esc to cancel"
	case d.busy:
		right = "esc to interrupt"
	case d.buf.placeholderBefore() > 0:
		right = "tab to open the paste"
	case d.buf.empty():
		right = "? for shortcuts"
	}
	row1 := "  " + left
	if right != "" {
		gap := w - displayWidth(row1) - displayWidth(right)
		if gap >= 2 {
			row1 += strings.Repeat(" ", gap) + s.Dim(right)
		} else if d.hint != "" {
			row1 = "  " + s.Dim(d.hint) // a hint is worth more than the label for its moment
		}
	}
	rows := []string{truncateWidth(row1, w)}

	if m.Line != "" {
		rows = append(rows, truncateWidth("  "+m.Line, w))
		return rows
	}
	if line := statusLine(s, m, w-2); line != "" {
		rows = append(rows, "  "+line)
	}
	return rows
}

// modeLabel names the permission mode in words and a mark, so it reads the
// same in any theme and without colour.
func modeLabel(s Style, m StatusModel) string {
	mode := m.Mode
	if mode == "" {
		mode = "default"
	}
	label := ""
	switch mode {
	case "accept-edits":
		label = s.Green("⏵⏵ accept edits")
	case "plan":
		label = s.Blue("⏸ plan mode")
	case "auto":
		label = s.Yellow("⏵⏵⏵ auto mode")
	case "bypass":
		label = s.Red("⚠ bypass mode")
	default:
		label = s.Dim("● default mode")
	}
	if m.ModeLocked {
		label += s.Dim(" (managed)")
	}
	if m.PendingMode != "" && m.PendingMode != mode {
		label += s.Dim(" → " + m.PendingMode + " after this turn")
	} else if !m.ModeLocked {
		label += s.Dim(" · shift+tab")
	}
	return label
}

// statusLine joins what is known, most useful first, dropping from the end
// what does not fit.
func statusLine(s Style, m StatusModel, w int) string {
	var parts []string
	if m.Model != "" {
		parts = append(parts, m.Model)
	}
	if m.SessionTitle != "" {
		parts = append(parts, "session "+truncateWidth(m.SessionTitle, 32))
	}
	if m.ContextTokens > 0 {
		p := fmt.Sprintf("%d%% context", m.ContextPercent)
		switch {
		case m.ContextPercent >= 90:
			p = s.Red(p)
		case m.ContextPercent >= 70:
			p = s.Yellow(p)
		}
		parts = append(parts, p)
	}
	if n := m.TokensIn + m.TokensOut; n > 0 {
		parts = append(parts, formatCount(n)+" tokens")
	}
	if m.CostUSD != nil {
		parts = append(parts, fmt.Sprintf("$%.2f", *m.CostUSD))
	}
	if m.BackgroundTasks > 0 {
		parts = append(parts, fmt.Sprintf("%d background", m.BackgroundTasks))
	}
	if m.WaitingAsk {
		parts = append(parts, s.Yellow("waiting for you"))
	}
	if m.Cwd != "" {
		loc := m.Cwd
		if m.GitBranch != "" {
			loc += " (" + m.GitBranch + ")"
		}
		parts = append(parts, loc)
	}
	out := ""
	for i, p := range parts {
		next := p
		if out != "" {
			next = out + " · " + p
		}
		if displayWidth(next) > w && i == len(parts)-1 && m.Cwd != "" {
			// Where the session is gives way from the left: the folder's own
			// name and the branch are what tell sessions apart.
			short := "…/" + filepath.Base(m.Cwd)
			if m.GitBranch != "" {
				short += " (" + m.GitBranch + ")"
			}
			next = short
			if out != "" {
				next = out + " · " + short
			}
		}
		if displayWidth(next) > w {
			break
		}
		out = next
	}
	return s.Dim(out)
}

// activityRow is the line shown while a turn runs:
//
//	⠹ Thinking… (12s · ↓ 1.2k tokens · esc to interrupt)
func (d *dock) activityRow(w int) string {
	s := d.st
	verb := thinkingVerbs[(d.act.verb)%len(thinkingVerbs)]
	what := verb + "…"
	if d.act.label != "" {
		what = d.act.label + "…"
	}
	meta := []string{formatElapsed(d.now().Sub(d.act.since))}
	if d.act.tokens > 0 {
		meta = append(meta, "↓ "+formatCount(d.act.tokens)+" tokens")
	}
	meta = append(meta, "esc to interrupt")
	frame := spinFrames[d.spin%len(spinFrames)]
	row := s.Accent(frame) + " " + s.Accent(what) + " " + s.Dim("("+strings.Join(meta, " · ")+")")
	for displayWidth(row) > w && len(meta) > 1 {
		meta = meta[:len(meta)-1]
		row = s.Accent(frame) + " " + s.Accent(what) + " " + s.Dim("("+strings.Join(meta, " · ")+")")
	}
	return truncateWidth(row, w)
}

func formatElapsed(d time.Duration) string {
	sec := int(d.Seconds())
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	return fmt.Sprintf("%dm %02ds", sec/60, sec%60)
}

func formatCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}
