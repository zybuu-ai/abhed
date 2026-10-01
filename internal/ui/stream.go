package ui

import "strings"

// mdStream renders a reply as it arrives, a fragment at a time.
//
// Each complete line is rendered with the markdown state the lines before it
// left — so a fenced block stays a block across fragments and its lines are
// never read as markdown — and committed. The line still arriving is shown
// in the dock as it grows; its rows above the last cannot change any more
// (a growing word only ever moves the last row), so they are committed too,
// and a long paragraph scrolls as it streams rather than sitting in the dock.
type mdStream struct {
	st        mdState
	pend      string
	committed int // rows of the pending line already committed
	src       strings.Builder
}

// feed adds a fragment and returns the rows now final and the rows to show
// in the dock until the next fragment.
func (m *mdStream) feed(s Style, text string, width int) (commit, live []string) {
	m.src.WriteString(text)
	m.pend += text
	for {
		i := strings.IndexByte(m.pend, '\n')
		if i < 0 {
			break
		}
		line := m.pend[:i]
		m.pend = m.pend[i+1:]
		rows := m.st.line(s, line, width)
		if m.committed > 0 {
			rows = rows[min(m.committed, len(rows)):]
			m.committed = 0
		}
		commit = append(commit, rows...)
	}
	for _, t := range m.st.table {
		live = append(live, s.Dim(truncateWidth(t, width)))
	}
	rows := m.st.peek(s, m.pend, width)
	rows = rows[min(m.committed, len(rows)):]
	if len(m.st.table) == 0 && len(rows) > 1 {
		commit = append(commit, rows[:len(rows)-1]...)
		m.committed += len(rows) - 1
		rows = rows[len(rows)-1:]
	}
	return commit, append(live, rows...)
}

// end renders what is left and returns it.
func (m *mdStream) end(s Style, width int) []string {
	var out []string
	if m.pend != "" {
		rows := m.st.line(s, m.pend, width)
		out = append(out, rows[min(m.committed, len(rows)):]...)
		m.pend, m.committed = "", 0
	}
	return append(out, m.st.flush(s, width)...)
}

// text is everything fed so far.
func (m *mdStream) text() string { return m.src.String() }
