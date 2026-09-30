// Package clitest drives the abhed binary in a pseudo-terminal against a
// scripted model, for end-to-end tests of the interactive CLI: what reaches
// the screen, how many bytes it took, how fast, and what the record holds.
//
// This file is the API every track writes its tests against. Until the
// harness is built, Start skips the test that calls it.
//
// A test looks like:
//
//	h := clitest.Start(t, clitest.Opts{Script: `text "Hello"`, Args: []string{"-C", ws}})
//	h.Type("fix it")
//	h.Key(clitest.Enter)
//	h.WaitText("Hello")
//	if got := h.Record().Types(); ... {}
//	h.Exit(0)
//
// The binary runs with HOME in a temporary directory, TERM=xterm-256color, a
// fixed clock, a stub podman on PATH and a config pointing at the stub model.
// There is no real model, proxy or network: only loopback.
package clitest

import (
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Opts configure one run.
type Opts struct {
	// Cols and Rows are the terminal size; zero means 80x24.
	Cols, Rows int
	// Script is what the stub model does, in the DSL below.
	Script Script
	// Args are the command-line arguments after "abhed".
	Args []string
	// Theme is dark, light or no-color ("" is dark).
	Theme string
	// Managed is the content of the managed config.json; "" runs unmanaged.
	Managed string
	// Env adds KEY=value pairs to the binary's environment.
	Env []string
	// Term replaces TERM, e.g. "dumb".
	Term string
	// NoSyncOutput makes the emulator report that it lacks synchronized
	// output (mode 2026), as some terminals do.
	NoSyncOutput bool
	// Piped runs with stdin and stdout as pipes instead of a terminal.
	Piped bool
}

// Script is the stub model's behaviour, one step per line:
//
//	text "Hello"             stream a reply
//	reasoning "..."          stream reasoning
//	tool edit {"path":"a"}   call a tool with these arguments
//	delay 30ms               wait before the next step
//	stall 5s                 send nothing for this long
//	error 500                answer with this HTTP status
//	usage in=1200 cached=900 report token usage
//
// A blank line ends one model turn; the next request gets the next turn.
type Script string

// Key is a key press, as the bytes a terminal sends for it.
type Key string

const (
	Enter      Key = "\r"
	Tab        Key = "\t"
	ShiftTab   Key = "\x1b[Z"
	Esc        Key = "\x1b"
	Up         Key = "\x1b[A"
	Down       Key = "\x1b[B"
	Right      Key = "\x1b[C"
	Left       Key = "\x1b[D"
	Backspace  Key = "\x7f"
	AltEnter   Key = "\x1b\r"
	ShiftEnter Key = "\x1b[13;2u"
	CtrlC      Key = "\x03"
	CtrlD      Key = "\x04"
	CtrlJ      Key = "\n"
	CtrlL      Key = "\x0c"
	CtrlO      Key = "\x0f"
	CtrlR      Key = "\x12"
	CtrlT      Key = "\x14"
)

// PasteMode is how Paste delivers text.
type PasteMode int

const (
	// Bracketed wraps the text in the bracketed-paste markers.
	Bracketed PasteMode = iota
	// Raw sends the text at once with no markers, as a terminal without
	// bracketed paste does.
	Raw
)

// Harness is one running binary.
type Harness interface {
	// Type sends text as typed keys.
	Type(text string)
	// Key sends key presses in order.
	Key(keys ...Key)
	// Paste sends text as one paste.
	Paste(text string, mode PasteMode)
	// Resize changes the terminal size and signals the binary.
	Resize(cols, rows int)

	// WaitScreen waits until cond holds for the screen and returns it; it
	// fails the test when timeout passes first.
	WaitScreen(cond func(Screen) bool, timeout time.Duration) Screen
	// WaitText waits, with the default timeout, until text is on the screen.
	WaitText(text string) Screen
	// Screen is the screen now.
	Screen() Screen
	// Scrollback is the lines that scrolled off the top, ANSI stripped.
	Scrollback() []string

	// MarkBytes starts counting the bytes the binary writes; BytesSinceMark
	// is the count since.
	MarkBytes()
	BytesSinceMark() int
	// Deltas are the stub model's streamed fragments, each with when it was
	// sent; Latency is how long after one the screen first changed.
	Deltas() []Delta
	Latency(d Delta) (time.Duration, bool)
	// Requests are what the binary sent the stub model, in order.
	Requests() []Request

	// Record is the session's record, read and verified.
	Record() Record
	// Exit closes input, waits for the binary to end and fails the test
	// unless it exits with code.
	Exit(code int)
}

// Screen is the emulator's view of the terminal.
type Screen interface {
	Cols() int
	Rows() int
	// Line is row i's text, 0 at the top, trailing spaces trimmed.
	Line(i int) string
	// Text is every row joined by newlines.
	Text() string
	// Contains reports whether text appears on any row.
	Contains(text string) bool
	// Cell is the character and style at a row and column.
	Cell(row, col int) Cell
	// Cursor is the cursor's row and column.
	Cursor() (row, col int)
	// Title is the last title the binary set.
	Title() string
}

// Cell is one character cell.
type Cell struct {
	Rune  rune
	Width int // 2 for a wide character's first cell, 0 for its second
	Attrs Attrs
}

// Attrs is a cell's style.
type Attrs struct {
	FG, BG                                string // "" for the default, else a palette index or #rrggbb
	Bold, Dim, Italic, Underline, Reverse bool
}

// Delta is one fragment the stub model streamed.
type Delta struct {
	Index  int
	Text   string
	SentAt time.Time
}

// Request is one request the stub model received.
type Request struct {
	At   time.Time
	Body []byte
}

// Record is a session's record as the harness read it.
type Record struct {
	Events []agent.Event
	// Verified is set when the record's chain verified.
	Verified bool
}

// Types lists the record's event types in order.
func (r Record) Types() []agent.EventType {
	out := make([]agent.EventType, len(r.Events))
	for i, e := range r.Events {
		out[i] = e.Type
	}
	return out
}

// Start builds the binary once per test process, runs it and returns the
// harness. It skips the test until the harness is built.
func Start(t testing.TB, o Opts) Harness {
	t.Helper()
	t.Skip("clitest: the pty harness is not built yet")
	return nil
}
