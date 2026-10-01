package termline

import (
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// The capture rebuilds a line from the keys typed, and says when it could not.
func TestCaptureRebuildsTypedLines(t *testing.T) {
	for keys, want := range map[string]struct {
		line   string
		edited bool
	}{
		"ls -la\r":                       {"ls -la", false},
		"ls -lx\x7fa\r":                  {"ls -la", true},
		"rm -rf x\x15echo hi\r":          {"echo hi", false},
		"\x1b[200~git status\x1b[201~\r": {"git status", false},
		"gi\tstatus\r":                   {"gistatus", true},
		"\x1b[A\r":                       {"", true},
		"echo one two\x17three\r":        {"echo one three", true},
	} {
		c := NewCapture("u1", nil)
		chunks := c.Keys([]byte(keys))
		e := chunks[len(chunks)-1].Enter
		if e == nil || e.Line != want.line || e.Edited != want.edited || !e.Whole {
			t.Errorf("%q: got %+v, want %q edited=%v", keys, e, want.line, want.edited)
		}
	}
	c := NewCapture("u1", nil)
	c.Output([]byte("\x1b[?1049h"))
	if chunks := c.Keys([]byte(":wq\r")); !chunks[0].Enter.Alt {
		t.Error("a full-screen program's keys were taken for a shell line")
	}
}

// When the capture cannot be sure a line was shown as typed, it keeps the line
// without its text.
func TestCaptureWithholdsWhenUnsure(t *testing.T) {
	ahead := func(e *Entered) *Entered { e.Ahead = true; return e }
	typed := func(keys string, echo string, known, secret bool) *Entered {
		c := NewCapture("u1", nil)
		for i := range keys[:len(keys)-1] {
			c.Keys([]byte{keys[i]})
			if i == 0 { // the terminal shows the given text once, as the line is typed
				c.Output([]byte(echo))
			}
		}
		e := c.Keys([]byte{keys[len(keys)-1]})[0].Enter
		e.Known, e.Secret = known, secret
		return e
	}
	for name, tc := range map[string]struct {
		e    *Entered
		want bool
	}{
		"echoed long line":        {typed("git status\r", "git status", false, false), true},
		"not echoed":              {typed("hunter22\r", "\r\n", true, false), false},
		"password mode":           {typed("git status\r", "git status", true, true), false},
		"short, terminal asked":   {typed("ls\r", "ls", true, false), true},
		"short, terminal unknown": {typed("ls\r", "ls", false, false), false},
		"edit at the Enter":       {typed("abcdX\x7fr\r", "abcdX", true, false), false},
		"only the end shown":      {typed("hunter22echo hi there\r", "echo hi there", true, false), false},
		"all but one key shown":   {typed("echo hi there\r", "echo hi the", true, false), false},
		"typed ahead":             {ahead(typed("hunter22\r", "hunter22", true, false)), false},
	} {
		if got := tc.e.Echoed(); got != tc.want {
			t.Errorf("%s: echoed() = %v, want %v (%+v)", name, got, tc.want, tc.e)
		}
	}
}

// A pasted line judged while the terminal reads unshown is withheld, though
// its echo arrived before echo was turned off.
func TestCaptureWithholdsWhileHidden(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		var got []agent.TerminalInput
		c := NewCapture("u1", func(in agent.TerminalInput) { got = append(got, in) })
		c.Hidden = func() bool { return hidden }
		e := c.Keys([]byte("hunter22\r"))[0].Enter
		e.Known = true
		c.Entered(e)
		c.Output([]byte("hunter22\r\n"))
		c.Flush()
		if len(got) != 1 || (got[0].Line == "hunter22") == hidden {
			t.Fatalf("hidden=%v: %+v", hidden, got)
		}
	}
}

// A switch to the alternate screen split across two reads is still seen.
func TestCaptureSeesASplitScreenSwitch(t *testing.T) {
	c := NewCapture("u1", nil)
	c.Output([]byte("vim\x1b[?10"))
	c.Output([]byte("49h~"))
	if !c.alt {
		t.Fatal("a switch split across reads was missed")
	}
}

// A flood of lines cannot turn into a flood of events.
func TestCaptureBoundsWhatWaits(t *testing.T) {
	var got []agent.TerminalInput
	var mu sync.Mutex
	c := NewCapture("u1", func(in agent.TerminalInput) { mu.Lock(); got = append(got, in); mu.Unlock() })
	for _, k := range c.Keys([]byte(strings.Repeat("abcdef\r", 500))) {
		c.Entered(k.Enter)
	}
	c.Flush()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != maxPending+1 || !strings.Contains(got[len(got)-1].Withheld, "436 more lines") {
		t.Fatalf("%d events, last %+v", len(got), got[len(got)-1])
	}
}

// Plain text keeps what a person reads and drops what only drives the screen.
func TestPlainTextStripsControl(t *testing.T) {
	if got := PlainText([]byte("\x1b[1mbold\x1b[0m\r\nnext\x07")); got != "bold\nnext" {
		t.Fatalf("%q", got)
	}
}

// A line withheld from the record is taken out of the output that is
// recorded too: typed ahead of read -s, the terminal echoed it before echo
// went off, and the shell's latest output would otherwise carry it.
func TestScrubTakesWithheldLinesOutOfOutput(t *testing.T) {
	c := NewCapture("u1", func(agent.TerminalInput) {})
	for _, keys := range []string{"hunter33\r", "ok\r"} {
		chunks := c.Keys([]byte(keys))
		e := chunks[len(chunks)-1].Enter
		e.Known, e.Ahead = true, true
		c.Entered(e)
	}
	c.Flush()
	got := c.Scrub("$ read -s pw\nhunter33\nalso 8\nok\nlooks ok\n$ ")
	if strings.Contains(got, "hunter33") || got != "$ read -s pw\n[withheld]\nalso 8\n[withheld]\nlooks ok\n$ " {
		t.Fatalf("scrubbed: %q", got)
	}
	// A line the terminal showed as typed is kept, in the record and the output.
	c = NewCapture("u1", func(agent.TerminalInput) {})
	c.Keys([]byte("g"))
	c.Output([]byte("git status"))
	e := c.Keys([]byte("it status\r"))[0].Enter
	e.Known = true
	c.Entered(e)
	if got := c.Scrub("git status\n"); got != "git status\n" {
		t.Fatalf("an echoed line was scrubbed: %q", got)
	}
}
