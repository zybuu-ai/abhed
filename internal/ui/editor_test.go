package ui

import (
	"bytes"
	"strings"
	"testing"
)

// drive feeds keystrokes to an editor and returns the line it produced plus
// everything it painted.
func drive(t *testing.T, keys string) (string, string) {
	t.Helper()
	var out bytes.Buffer
	e := newEditor(strings.NewReader(keys), &out, "> ")
	line, err := e.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	return line, out.String()
}

// TestBackspaceClosesTheMenu pins the bug that prompted this editor.
//
// x/term's AutoCompleteCallback never sees Backspace, and the line it passes
// is the one BEFORE the key was applied, so erasing a slash command left its
// menu on screen with nothing to select.
func TestBackspaceClosesTheMenu(t *testing.T) {
	// "/co", then three backspaces to empty, then Enter.
	line, painted := drive(t, "/co\x7f\x7f\x7f\r")
	if line != "" {
		t.Fatalf("line should be empty, got %q", line)
	}
	// The final paint must not contain a command row.
	last := painted[strings.LastIndex(painted, "\r\033[J"):]
	for _, c := range []string{"/compact", "/cost"} {
		if strings.Contains(last, c) {
			t.Errorf("menu still shown after erasing the line: %q appears", c)
		}
	}
}

// TestArrowKeysMoveThroughTheMenu pins the second bug: arrows were consumed by
// x/term for history and never reached the suggestions.
func TestArrowKeysMoveThroughTheMenu(t *testing.T) {
	matches := MatchCommands("/c")
	if len(matches) < 1 {
		t.Skip("no /c commands")
	}
	// One Down highlights the first candidate; Enter takes it.
	line, _ := drive(t, "/c\x1b[B\r")
	want := matches[0].Name
	if !strings.HasPrefix(line, want) {
		t.Errorf("Down then Enter gave %q, want the first candidate %q", line, want)
	}
}

// TestChoosingACommandThatTakesAnArgumentWaits: Enter on such a command fills
// the line and stops, because submitting "/mode" with no mode is not what
// choosing it meant. A command with no argument submits immediately.
func TestChoosingACommandThatTakesAnArgumentWaits(t *testing.T) {
	// /compact takes [hint]: Enter fills it, then "now" and Enter submit.
	line, _ := drive(t, "/comp\x1b[B\rnow\r")
	if line != "/compact now" {
		t.Errorf("got %q, want %q", line, "/compact now")
	}
	// /cwd takes nothing: one Enter is the whole gesture.
	line2, _ := drive(t, "/cwd\x1b[B\r")
	if strings.TrimSpace(line2) != "/cwd" {
		t.Errorf("got %q, want /cwd", line2)
	}
}

// TestSelectionWrapsAndWrapsBack keeps the list navigable from either end.
func TestSelectionWrapsAndWrapsBack(t *testing.T) {
	// "/c", Up once — selects the LAST candidate, not nothing.
	line, _ := drive(t, "/c\x1b[A\r")
	matches := MatchCommands("/c")
	want := matches[len(matches)-1].Name
	if !strings.HasPrefix(line, want) {
		t.Errorf("Up from no selection gave %q, want the last candidate %q", line, want)
	}
}

// TestTabCompletesToTheSharedStem keeps Tab useful when the choice is
// ambiguous rather than doing nothing.
func TestTabCompletesToTheSharedStem(t *testing.T) {
	line, _ := drive(t, "/co\t\r")
	if line != "/co" && !strings.HasPrefix(line, "/co") {
		t.Errorf("Tab on an ambiguous stem gave %q", line)
	}
	// Unambiguous: completes the whole command.
	line2, _ := drive(t, "/quit\t\r")
	if !strings.HasPrefix(line2, "/quit") {
		t.Errorf("Tab on a unique match gave %q, want /quit", line2)
	}
}

// TestOrdinaryTextNeverOpensTheMenu: a slash in a sentence or a path is not a
// command.
func TestOrdinaryTextNeverOpensTheMenu(t *testing.T) {
	_, painted := drive(t, "ls /tmp\r")
	if strings.Contains(painted, "/compact") {
		t.Error("a path opened the command menu")
	}
}

// TestEditingKeysBehave covers the ordinary line editing the prompt needs.
func TestEditingKeysBehave(t *testing.T) {
	// "abc", Left, "X" -> "abXc"
	if line, _ := drive(t, "abc\x1b[DX\r"); line != "abXc" {
		t.Errorf("Left then insert gave %q, want abXc", line)
	}
	// Ctrl-A then "X" -> prepends
	if line, _ := drive(t, "abc\x01X\r"); line != "Xabc" {
		t.Errorf("Ctrl-A then insert gave %q, want Xabc", line)
	}
	// Ctrl-U clears to start
	if line, _ := drive(t, "abc\x15z\r"); line != "z" {
		t.Errorf("Ctrl-U gave %q, want z", line)
	}
	// Ctrl-W deletes a word
	if line, _ := drive(t, "go test ./...\x17\r"); line != "go test " {
		t.Errorf("Ctrl-W gave %q, want 'go test '", line)
	}
}

// TestQuietSuppressesThePrompt pins the stacked-glyph bug.
//
// The reader goroutine keeps reading during a turn so a steering message can
// be typed. It must not paint a prompt while doing so: every write during a
// turn repainted one, which stacked a column of prompt glyphs under each
// answer and painted over the thinking indicator.
func TestQuietSuppressesThePrompt(t *testing.T) {
	var out bytes.Buffer
	e := newEditor(strings.NewReader(""), &out, "PROMPT> ")
	e.reading = true

	e.setQuiet(true)
	out.Reset()
	e.redraw()
	if strings.Contains(out.String(), "PROMPT>") {
		t.Error("the prompt was painted while a turn owns the screen")
	}

	e.setQuiet(false)
	if !strings.Contains(out.String(), "PROMPT>") {
		t.Error("the prompt did not come back when the turn finished")
	}
}

// TestWritePassesThroughOutsideAnEdit: while no line is being edited the
// editor must not erase and repaint anything, or it overwrites the output it
// was asked to print.
func TestWritePassesThroughOutsideAnEdit(t *testing.T) {
	var out bytes.Buffer
	e := newEditor(strings.NewReader(""), &out, "> ")
	e.reading = false // no line in progress, as during a turn

	if _, err := e.write([]byte("spinner frame")); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "\033[J") {
		t.Error("write erased the screen while no line was being edited")
	}
	if !strings.Contains(got, "spinner frame") {
		t.Error("write dropped its output")
	}
}
