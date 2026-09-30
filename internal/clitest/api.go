// Package clitest drives the abhed binary in a pseudo-terminal against a
// scripted model, for end-to-end tests of the interactive CLI: what reaches
// the screen, how many bytes it took, how fast, and what the record holds.
//
// This file is the API every track writes its tests against; harness.go,
// stub.go, record.go, golden.go and budget.go implement it, and vt is the
// terminal emulator the screen comes from.
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
// The binary is built once per test process (with -race when the tests
// are). It runs with HOME and the workspace in a temporary directory,
// TERM=xterm-256color, LANG=en_US.UTF-8, a stub podman first on a minimal
// PATH, proxies pointing at a closed port, and a user config naming the
// stub model. There is no real model, proxy or network: only loopback.
// The environment is built from nothing, so no variable of the person
// running the tests reaches the binary.
//
// A test that waits on another track's work calls Pending with the plan
// item; CI accepts a skip only for the items listed in ABHED_CLITEST_PENDING
// in the workflow, so a test that stops running fails the build.
//
// In the strings of Args, Env, UserConfig and Managed, {{MODEL_URL}} is the
// stub's base URL, {{HOME}} the run's HOME and {{WS}} its workspace.
package clitest

import (
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/clitest/vt"
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
	// Stdin, with Piped, is written to the binary's stdin, which is then
	// closed unless KeepStdin is set; Type then writes more.
	Stdin     string
	KeepStdin bool

	// NoConfig starts with no user config at all, as a first run does.
	NoConfig bool
	// UserConfig replaces DefaultUserConfig as ~/.abhed/config.json.
	UserConfig string
	// Podman is the body of the stub podman script ("" exits 1 at once);
	// "sleep 3" makes a slow one.
	Podman string
	// Setup runs after the files are written and before the binary starts,
	// to add files to the home or the workspace.
	Setup func(home, ws string)
}

// Script is the stub model's behaviour, one step per line:
//
//	text "Hello"             stream a reply
//	reasoning "..."          stream reasoning
//	tool edit {"path":"a"}   call a tool with these arguments
//	delay 30ms               wait before the next step
//	stall 5s                 send nothing for this long
//	error 500                answer with this HTTP status
//	usage in=1200 cached=900 out=40   report token usage
//	# a comment
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

	// Record is the session's record, read and verified. Until the local
	// record exists it marks the test pending on C1.
	Record() Record
	// Exit closes input, waits for the binary to end and fails the test
	// unless it exits with code.
	Exit(code int)

	// Wait waits for the binary to end on its own and returns its code.
	Wait(timeout time.Duration) int
	// WaitOutput waits until the ANSI-stripped output holds text, wherever
	// it has scrolled to, and returns how long after the spawn it appeared.
	WaitOutput(text string) time.Duration
	// FirstSeen is how long after the spawn the output first held text.
	FirstSeen(text string) (time.Duration, bool)
	// WaitQuiet waits until nothing is written for quiet, at most max;
	// WaitSettled first waits for a write after since.
	WaitQuiet(quiet, max time.Duration)
	// Settle waits for the binary to stop writing, before typing the next
	// command.
	Settle()
	WaitSettled(since time.Time, quiet, max time.Duration)
	// Output is every byte written; Stdout and Stderr are the streams of a
	// piped run.
	Output() []byte
	Stdout() string
	Stderr() string
	// Home and Workspace are the run's directories; Started is the spawn.
	Home() string
	Workspace() string
	Started() time.Time
	// Stub is the scripted model.
	Stub() *Stub

	// Normalize replaces the run's directories, times, token counts, ids,
	// hashes and spinner frames with placeholders.
	Normalize(text string) string
	// ScreenGolden and AttrsGolden are the normalized screen (after the
	// scrollback) and its style runs, drawn again with the run directory at
	// a fixed width; AssertGoldens compares them, and the record golden when
	// evs is not nil, with testdata/golden/<scenario>/ (-update rewrites).
	ScreenGolden() string
	AttrsGolden() string
	AssertGoldens(scenario string, evs []agent.Event)
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
	// Modes are the terminal modes the binary set: bracketed paste,
	// synchronized output, the alternate screen, cursor visibility.
	Modes() vt.Modes
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
