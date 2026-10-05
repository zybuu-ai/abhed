package ui

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func sampleWork(n int) []WorkRow {
	rows := []WorkRow{{State: WorkRunning, Elapsed: 90 * time.Second, Tokens: 12000}}
	for i := range n {
		rows = append(rows, WorkRow{ID: string(rune('a' + i)), Depth: 1, Kind: "general", Title: "task " + string(rune('a'+i)),
			Activity: "bash go test", State: WorkRunning, Elapsed: 48 * time.Second, Tokens: 128_200, Target: true})
	}
	return rows
}

// The panel lists the conversation first, then each row with its mark,
// kind, title, activity, elapsed time and tokens, every row within width.
func TestWorkPanelRows(t *testing.T) {
	rows := sampleWork(2)
	rows[2].State, rows[2].Activity = WorkFailed, ""
	got := workPanel(Style{}, rows, "", 80)
	text := strings.Join(got, "\n")
	for _, want := range []string{"↑/↓ to select · Enter to view", "❯ ● main", "1m 30s · 12k tokens in",
		"● general  task a · bash go test", "48s · 128k tokens in", "✕ general  task b"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in:\n%s", want, text)
		}
	}
	for _, r := range got {
		if w := displayWidth(r); w > 80 {
			t.Errorf("row is %d wide: %q", w, r)
		}
	}
	if workPanel(Style{}, rows[:1], "", 80) != nil {
		t.Error("a panel was drawn with nothing but the conversation")
	}
}

// Past five rows the rest are counted, and the window follows the selection.
func TestWorkPanelMore(t *testing.T) {
	rows := sampleWork(8)
	text := strings.Join(workPanel(Style{}, rows, "", 80), "\n")
	if !strings.Contains(text, "↓ 3 more") || strings.Contains(text, "task f") {
		t.Fatalf("eight rows did not fold to five:\n%s", text)
	}
	text = strings.Join(workPanel(Style{}, rows, "h", 80), "\n")
	if !strings.Contains(text, "↑ 3 more") || !strings.Contains(text, "❯ ● general  task h") || strings.Count(text, "more") != 1 {
		t.Fatalf("the window did not follow the selection:\n%s", text)
	}
}

// A nested row is drawn under its parent; a narrow terminal drops the
// right-hand column before the title, and nothing is wider than the row.
func TestWorkPanelNestingAndWidth(t *testing.T) {
	rows := sampleWork(2)
	rows[2].Depth = 2
	text := strings.Join(workPanel(Style{}, rows, "", 80), "\n")
	if !strings.Contains(text, "  └ ● general  task b") {
		t.Fatalf("no nested row:\n%s", text)
	}
	for _, w := range []int{20, 30, 44} {
		for _, r := range workPanel(Style{}, rows, "a", w) {
			if displayWidth(r) > w {
				t.Errorf("at %d: %q is %d wide", w, r, displayWidth(r))
			}
		}
	}
	narrow := strings.Join(workPanel(Style{}, rows, "", 30), "\n")
	if strings.Contains(narrow, "tokens in") {
		t.Errorf("tokens kept at 30 columns:\n%s", narrow)
	}
}

// A title holding escapes or newlines is drawn as text.
func TestWorkPanelSanitizes(t *testing.T) {
	rows := sampleWork(1)
	rows[1].Title = "evil\x1b[2J\ntitle"
	for _, r := range workPanel(Style{}, rows, "", 80) {
		if strings.Contains(r, "\x1b[2J") || strings.Contains(r, "\n") {
			t.Fatalf("an escape or newline was drawn: %q", r)
		}
	}
	// Hidden characters are shown as escapes, as /tasks writes them, not dropped.
	rows[1].Title = "ab\u202ecd\u200def"
	panel := strings.Join(workPanel(Style{}, rows, "", 120), "\n")
	if !strings.Contains(panel, VisibleLine(rows[1].Title)) || strings.Contains(panel, "\u202e") {
		t.Fatalf("the title's hidden characters were not shown: %q", panel)
	}
}

type workRig struct {
	*rig
	mu    sync.Mutex
	rows  []WorkRow
	opens []string
	sent  []string
	take  bool
}

func newWorkRig(t *testing.T) *workRig {
	w := &workRig{rig: newRig(t, 80, 24), rows: sampleWork(2), take: true}
	w.lr.SetWork(func() []WorkRow {
		w.mu.Lock()
		defer w.mu.Unlock()
		return append([]WorkRow(nil), w.rows...)
	}, func(id string) {
		w.mu.Lock()
		w.opens = append(w.opens, id)
		w.mu.Unlock()
	}, func(id, text string) bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.sent = append(w.sent, id+":"+text)
		return w.take
	})
	return w
}

// The arrows select only with nothing typed; Up from the conversation is
// still history; Esc goes back to the conversation.
func TestWorkSelectionArrows(t *testing.T) {
	g := newWorkRig(t)
	g.lr.d.mu.Lock()
	g.lr.d.hist.Add("earlier", false)
	g.lr.d.hpos = 1
	g.lr.d.mu.Unlock()
	g.waitText("↑/↓ to select")

	g.keys("abc")
	g.settle()
	g.keys("\x1b[B")
	g.settle()
	if g.lr.Selected() != "" {
		t.Fatal("Down selected a row while something was typed")
	}
	g.keys("\x15") // Ctrl-U clears the line
	g.settle()
	g.keys("\x1b[B")
	g.waitText("Message @general…")
	if g.lr.Selected() != "a" {
		t.Fatalf("selected %q", g.lr.Selected())
	}
	g.keys("\x1b[A")
	g.settle()
	if g.lr.Selected() != "" {
		t.Fatalf("Up did not go back to main: %q", g.lr.Selected())
	}
	g.keys("\x1b[A") // from main, Up is history
	g.waitText("earlier")
	g.keys("\x15\x1b[B")
	g.settle()
	g.keys("\x1b")
	time.Sleep(50 * time.Millisecond)
	g.settle()
	if g.lr.Selected() != "" {
		t.Fatalf("Esc kept the selection: %q", g.lr.Selected())
	}
}

// Enter on an empty line opens the selected row; a typed message goes to
// it, and a command still goes to the session.
func TestWorkEnterOpensAndSends(t *testing.T) {
	g := newWorkRig(t)
	g.waitText("↑/↓ to select")
	g.keys("\x1b[B")
	g.settle()
	g.keys("\r")
	g.waitScreen("an open", func(string) bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return len(g.opens) == 1 && g.opens[0] == "a"
	})
	g.typed("hurry up")
	g.keys("\r")
	g.waitText("@general ")
	g.mu.Lock()
	sent := append([]string(nil), g.sent...)
	g.mu.Unlock()
	if len(sent) != 1 || sent[0] != "a:hurry up" {
		t.Fatalf("sent %v", sent)
	}
	g.typed("/help")
	g.settle()
	g.keys("\r")
	if got, _ := g.line(); got != "/help" {
		t.Fatalf("a command went elsewhere: %q", got)
	}
}

// A row that cannot take a message says so, and the message goes to the
// conversation.
func TestWorkMessageFallsBackToMain(t *testing.T) {
	g := newWorkRig(t)
	g.mu.Lock()
	g.rows[1].Target = false
	g.mu.Unlock()
	g.waitText("↑/↓ to select")
	g.keys("\x1b[B")
	g.waitText("general can't take messages; a message goes to main")
	g.typed("hello")
	g.keys("\r")
	if got, _ := g.line(); got != "hello" {
		t.Fatalf("got %q", got)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.sent) != 0 {
		t.Fatalf("sent to a row that takes none: %v", g.sent)
	}
}

// With only the conversation, no panel and no hint are drawn, and the
// arrows are history as before.
func TestWorkNoPanelWithoutRows(t *testing.T) {
	g := newWorkRig(t)
	g.mu.Lock()
	g.rows = g.rows[:1]
	g.mu.Unlock()
	g.lr.Repaint()
	g.settle()
	if strings.Contains(g.term.Text(), "to select") {
		t.Fatalf("a hint with no rows:\n%s", g.term.Dump())
	}
	g.keys("\x1b[B")
	g.settle()
	if g.lr.Selected() != "" {
		t.Fatal("selected with no rows")
	}
}
