// Package termline follows an interactive terminal's keys and output closely
// enough to rebuild each line a person enters, judge it before the shell gets
// it, and record it, withheld when the terminal did not show it. The
// workbench and the editor protocol's Abhed terminal share it.
package termline

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// An interactive shell cannot be judged call by call: the shell decides what
// a line means, and completion, history and aliases happen inside it. So the
// sandbox is the terminal's boundary. What a surface can still do, from the
// keys it forwards, is rebuild each line as typed, put it to the deny rules
// before the Enter reaches the shell, and record it. Both are best effort, and
// the docs say so (docs/guide/16-workbench.md). Where it cannot tell whether a
// line was a secret, it records the line without its text.

const (
	// MaxLine bounds one line typed into a terminal.
	MaxLine = 8 << 10
	// EchoWait is how long a pasted line's echo has to arrive before it is judged.
	EchoWait = 300 * time.Millisecond
	// echoKeep bounds the output kept to find a line's echo in.
	echoKeep = 8 << 10
	// probeMin is the shortest line kept where the terminal cannot be asked.
	probeMin = 4
	// maxPending bounds the lines waiting to be judged; more are counted, not kept.
	maxPending = 64
	// withheldEcho is why a line's text is not in the record.
	withheldEcho = "the terminal did not show this line as typed, so its text is not recorded"
	// maxHeld bounds the withheld lines kept to scrub from recorded output.
	maxHeld = 256
	// scrubbed stands in recorded output for a withheld line's text.
	scrubbed = "[withheld]"
)

// Entered is one line, at the Enter that submitted it.
type Entered struct {
	Line   string
	Edited bool
	// typedEcho is whether any output arrived while the line was typed.
	typedEcho bool
	// Whole is set when every key of the line came in one write, so none of it
	// reached the shell before the Enter did: a paste.
	Whole bool
	// echo is the output while the line was typed; for a whole line, the
	// output just after its Enter.
	echo []byte
	// Known is set when the terminal was asked at the Enter (process and none
	// tiers). Secret is canonical mode, in which bash is not at its prompt;
	// Program is another process group in the foreground than the shell.
	Known, Secret, Program bool
	// Alt is the container tier's guess at a full-screen program.
	Alt bool
	// Ahead is set when the line came before the shell was back at its
	// prompt: whatever reads it may have turned echo off after it arrived.
	Ahead bool
}

// Chunk is input to forward as it is; Enter, when set, is the line its
// final byte submits.
type Chunk struct {
	Data  []byte
	Enter *Entered
}

// Capture follows a terminal's input and output closely enough to rebuild
// the lines a person enters and to tell whether the terminal echoed them.
type Capture struct {
	mu    sync.Mutex
	line  []byte
	esc   int // 0 none, 1 after ESC, 2 in a CSI sequence, 3 after ESC O
	param []byte
	edit  bool
	// started is set by the first key of a line; echo collects the output
	// from then on.
	started   bool
	typedEcho bool
	echo      []byte
	// alt follows the alternate screen, where the terminal cannot be asked;
	// tail keeps the end of the last read, for a switch split across two.
	alt     bool
	tail    []byte
	pending []*Entered
	dropped int
	record  func(agent.TerminalInput)
	callID  string
	// held are the texts of lines withheld from the record. The terminal may
	// still have echoed one, as when it was typed ahead of read -s turning
	// echo off, so Scrub takes them out of any output that is recorded.
	held []string
	// Hidden, when set, says whether a line is being read unshown now; a
	// pasted line judged while it is, is withheld.
	Hidden func() bool
}

// NewCapture follows one shell, recording each line it judges with record.
func NewCapture(callID string, record func(agent.TerminalInput)) *Capture {
	return &Capture{callID: callID, record: record}
}

// isSubmit is a key that hands the line to the shell: Enter, and Ctrl-O,
// which bash runs as operate-and-get-next.
func isSubmit(b byte) bool { return b == '\r' || b == '\n' || b == 0x0f }

// Keys splits typed input at each submit and rebuilds the line each one hands over.
func (c *Capture) Keys(data []byte) []Chunk {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Chunk
	from, here := 0, false
	for i, b := range data {
		if !c.started {
			c.started, c.typedEcho, c.echo, here = true, false, nil, true
		}
		switch c.esc {
		case 1:
			switch b {
			case '[':
				c.esc, c.param = 2, c.param[:0]
			case 'O':
				c.esc = 3
			default: // Alt with a key: the shell's to interpret
				c.esc = 0
				c.edited()
			}
			continue
		case 2:
			if b >= 0x40 && b <= 0x7e {
				c.esc = 0
				// Bracketed paste markers wrap text; they edit nothing.
				if p := string(c.param) + string(b); p != "200~" && p != "201~" {
					c.edited()
				}
			} else {
				c.param = append(c.param, b)
			}
			continue
		case 3:
			c.esc = 0
			c.edited()
			continue
		}
		switch {
		case isSubmit(b):
			e := c.submit(here)
			here = false
			out = append(out, Chunk{Data: data[from : i+1], Enter: e})
			from = i + 1
		case b == 0x1b:
			c.esc = 1
		case b == 0x7f || b == 0x08:
			if _, n := utf8.DecodeLastRune(c.line); n > 0 {
				c.line = c.line[:len(c.line)-n]
			}
			c.edited()
		case b == 0x17: // Ctrl-W: the word before the cursor
			c.line = bytes.TrimRight(c.line, " ")
			if j := bytes.LastIndexByte(c.line, ' '); j >= 0 {
				c.line = c.line[:j+1]
			} else {
				c.line = c.line[:0]
			}
			c.edited()
		case b == 0x03 || b == 0x15: // Ctrl-C and Ctrl-U abandon the line
			c.line, c.edit = c.line[:0], false
		case b < 0x20:
			c.edited()
		default:
			if len(c.line) < MaxLine {
				c.line = append(c.line, b)
			} else {
				c.edit = true
			}
		}
	}
	if from < len(data) {
		out = append(out, Chunk{Data: data[from:]})
	}
	return out
}

// Abandon forgets the line being typed.
func (c *Capture) Abandon() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.line, c.edit, c.started = c.line[:0], false, false
}

// edited marks the line as changed by a key the capture cannot follow.
func (c *Capture) edited() {
	c.edit = true
}

// submit ends the current line. Called with c.mu held. A typed line is
// matched against the output that came before its Enter; a pasted one,
// against what follows.
func (c *Capture) submit(whole bool) *Entered {
	e := &Entered{Line: string(c.line), Edited: c.edit, Whole: whole, typedEcho: c.typedEcho, Alt: c.alt}
	if !whole {
		e.echo = append([]byte(nil), c.echo...)
	}
	c.line, c.edit, c.started = c.line[:0], false, false
	return e
}

// Output follows what the terminal wrote: the echo of the line being typed,
// the echo of pasted lines waiting to be judged, and the alternate screen.
func (c *Capture) Output(chunk []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	joined := append(append([]byte(nil), c.tail...), chunk...)
	c.alt = altScreen(joined, c.alt)
	c.tail = append([]byte(nil), joined[max(0, len(joined)-8):]...)
	if c.started {
		c.typedEcho = true
		c.echo = KeepTail(append(c.echo, chunk...), echoKeep)
	}
	for _, p := range c.pending {
		if room := echoKeep - len(p.echo); p.Whole && room > 0 {
			p.echo = append(p.echo, chunk[:min(room, len(chunk))]...)
		}
	}
}

// Entered queues a line that reached the shell, to be judged and recorded:
// at once when typed, after its echo has had time to arrive when pasted.
func (c *Capture) Entered(e *Entered) {
	if e.Program || (e.Line == "" && !e.Edited) {
		return // keys for a program other than the shell, or an empty line
	}
	c.mu.Lock()
	if len(c.pending) >= maxPending {
		c.dropped++
		c.hold(e.Line)
		c.mu.Unlock()
		return
	}
	c.pending = append(c.pending, e)
	c.mu.Unlock()
	if !e.Whole {
		c.judge(e)
		return
	}
	time.AfterFunc(EchoWait, func() { c.judge(e) })
}

// Flush judges every line still waiting, when the shell has ended.
func (c *Capture) Flush() {
	c.mu.Lock()
	waiting := append([]*Entered(nil), c.pending...)
	c.mu.Unlock()
	for _, e := range waiting {
		c.judge(e)
	}
}

// judge records a line once, without its text unless the terminal showed it.
func (c *Capture) judge(e *Entered) {
	c.mu.Lock()
	found := false
	for i, p := range c.pending {
		if p == e {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			found = true
			break
		}
	}
	dropped := 0
	if len(c.pending) == 0 {
		dropped, c.dropped = c.dropped, 0
	}
	c.mu.Unlock()
	if found {
		in := agent.TerminalInput{CallID: c.callID, Line: e.Line, Edited: e.Edited}
		if !e.Echoed() || (e.Whole && c.Hidden != nil && c.Hidden()) {
			in = agent.TerminalInput{CallID: c.callID, Withheld: withheldEcho}
			c.mu.Lock()
			c.hold(e.Line)
			c.mu.Unlock()
		}
		c.record(in)
	}
	if dropped > 0 {
		c.record(agent.TerminalInput{CallID: c.callID,
			Withheld: fmt.Sprintf("%d more lines came faster than they could be followed, and are not recorded", dropped)})
	}
}

// hold keeps a withheld line's text for Scrub. Called with c.mu held.
func (c *Capture) hold(line string) {
	if strings.TrimSpace(line) == "" || slices.Contains(c.held, line) {
		return
	}
	if len(c.held) >= maxHeld {
		c.held = c.held[1:]
	}
	c.held = append(c.held, line)
}

// Scrub takes the text of every line withheld so far out of text, output
// about to be recorded. A line of probeMin bytes or more goes wherever it
// stands; a shorter one only where it stands alone on a line, so a one-letter
// answer does not take every such letter out of the output. Lines still
// waiting to be judged are judged first by Flush, which the caller runs.
func (c *Capture) Scrub(text string) string {
	c.mu.Lock()
	held := slices.Clone(c.held)
	c.mu.Unlock()
	// Longest first, so a line that contains another is taken whole.
	slices.SortFunc(held, func(a, b string) int { return len(b) - len(a) })
	for _, h := range held {
		if len(h) >= probeMin {
			text = strings.ReplaceAll(text, h, scrubbed)
		}
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if t := strings.TrimSpace(l); t != "" && slices.Contains(held, t) {
			lines[i] = scrubbed
		}
	}
	return strings.Join(lines, "\n")
}

// Echoed reports whether the terminal showed the line as it was typed: the
// whole line, unedited, in its echo. Keys a program read without an Enter
// (read -s -n) stay at the front of the next line, and a line that begins
// with them never matches. When unsure it says no: a line wrongly withheld
// costs the record its text, a line wrongly kept can put a password in it.
func (e *Entered) Echoed() bool {
	if e.Secret || e.Ahead || e.Edited || e.Line == "" || !strings.Contains(PlainText(e.echo), e.Line) {
		return false
	}
	// A short line needs the terminal to have said it was not reading a password.
	return len(e.Line) >= probeMin || e.Known
}

// altScreen follows the switches to and from a full-screen program's screen.
func altScreen(chunk []byte, was bool) bool {
	on, off := -1, -1
	for _, m := range []string{"\x1b[?1049h", "\x1b[?1047h", "\x1b[?47h"} {
		on = max(on, bytes.LastIndex(chunk, []byte(m)))
	}
	for _, m := range []string{"\x1b[?1049l", "\x1b[?1047l", "\x1b[?47l"} {
		off = max(off, bytes.LastIndex(chunk, []byte(m)))
	}
	switch {
	case on > off:
		return true
	case off > on:
		return false
	}
	return was
}

// KeepTail is the last n bytes of b.
func KeepTail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return append([]byte(nil), b[len(b)-n:]...)
}

// ansiSeq matches CSI, OSC and DCS sequences, charset selections, the
// single-character escapes, and the control bytes that only move a cursor.
var ansiSeq = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]|\x1b[P\\]^_][^\x1b\x07]*(\x07|\x1b\\\\)|\x1b[()*+][A-Za-z0-9]|\x1b[=>78cMDEH]|[\r\x00-\x08\x0b-\x0c\x0e-\x1a\x1c-\x1f]")

// PlainText strips terminal control sequences so the record reads as text.
func PlainText(b []byte) string {
	return string(ansiSeq.ReplaceAll(b, nil))
}
