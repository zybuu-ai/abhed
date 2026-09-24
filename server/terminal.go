package server

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// An interactive shell cannot be judged call by call: the shell decides what
// a line means, and completion, history and aliases happen inside it. So the
// sandbox is the terminal's boundary. What the server can still do, from the
// keys it forwards, is rebuild each line as typed, put it to the deny rules
// before the Enter reaches the shell, and record it. Both are best effort, and
// the docs say so (docs/guide/16-workbench.md). Where it cannot tell whether a
// line was a secret, it records the line without its text.

const (
	// echoWait is how long a pasted line's echo has to arrive before it is judged.
	echoWait = 300 * time.Millisecond
	// echoKeep bounds the output kept to find a line's echo in.
	echoKeep = 8 << 10
	// probeMin is the shortest line kept where the terminal cannot be asked.
	probeMin = 4
	// maxPending bounds the lines waiting to be judged; more are counted, not kept.
	maxPending = 64
	// withheldEcho is why a line's text is not in the record.
	withheldEcho = "the terminal did not show this line as typed, so its text is not recorded"
)

// enteredLine is one line, at the Enter that submitted it.
type enteredLine struct {
	line   string
	edited bool
	// typedEcho is whether any output arrived while the line was typed.
	typedEcho bool
	// whole is set when every key of the line came in one write, so none of it
	// reached the shell before the Enter did: a paste.
	whole bool
	// echo is the output while the line was typed; for a whole line, the
	// output just after its Enter.
	echo []byte
	// known is set when the terminal was asked at the Enter (process and none
	// tiers). secret is canonical mode, in which bash is not at its prompt;
	// program is another process group in the foreground than the shell.
	known, secret, program bool
	// alt is the container tier's guess at a full-screen program.
	alt bool
}

// keyChunk is input to forward as it is; enter, when set, is the line its
// final byte submits.
type keyChunk struct {
	data  []byte
	enter *enteredLine
}

// lineCapture follows a terminal's input and output closely enough to rebuild
// the lines a person enters and to tell whether the terminal echoed them.
type lineCapture struct {
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
	pending []*enteredLine
	dropped int
	record  func(agent.TerminalInput)
	callID  string
}

func newLineCapture(callID string, record func(agent.TerminalInput)) *lineCapture {
	return &lineCapture{callID: callID, record: record}
}

// isSubmit is a key that hands the line to the shell: Enter, and Ctrl-O,
// which bash runs as operate-and-get-next.
func isSubmit(b byte) bool { return b == '\r' || b == '\n' || b == 0x0f }

// keys splits typed input at each submit and rebuilds the line each one hands over.
func (c *lineCapture) keys(data []byte) []keyChunk {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []keyChunk
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
			out = append(out, keyChunk{data: data[from : i+1], enter: e})
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
			if len(c.line) < maxManualCommand {
				c.line = append(c.line, b)
			} else {
				c.edit = true
			}
		}
	}
	if from < len(data) {
		out = append(out, keyChunk{data: data[from:]})
	}
	return out
}

// abandon forgets the line being typed.
func (c *lineCapture) abandon() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.line, c.edit, c.started = c.line[:0], false, false
}

// edited marks the line as changed by a key the capture cannot follow.
func (c *lineCapture) edited() {
	c.edit = true
}

// submit ends the current line. Called with c.mu held. A typed line is
// matched against the output that came before its Enter; a pasted one,
// against what follows.
func (c *lineCapture) submit(whole bool) *enteredLine {
	e := &enteredLine{line: string(c.line), edited: c.edit, whole: whole, typedEcho: c.typedEcho, alt: c.alt}
	if !whole {
		e.echo = append([]byte(nil), c.echo...)
	}
	c.line, c.edit, c.started = c.line[:0], false, false
	return e
}

// output follows what the terminal wrote: the echo of the line being typed,
// the echo of pasted lines waiting to be judged, and the alternate screen.
func (c *lineCapture) output(chunk []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	joined := append(append([]byte(nil), c.tail...), chunk...)
	c.alt = altScreen(joined, c.alt)
	c.tail = append([]byte(nil), joined[max(0, len(joined)-8):]...)
	if c.started {
		c.typedEcho = true
		c.echo = keepTail(append(c.echo, chunk...), echoKeep)
	}
	for _, p := range c.pending {
		if room := echoKeep - len(p.echo); p.whole && room > 0 {
			p.echo = append(p.echo, chunk[:min(room, len(chunk))]...)
		}
	}
}

// entered queues a line that reached the shell, to be judged and recorded:
// at once when typed, after its echo has had time to arrive when pasted.
func (c *lineCapture) entered(e *enteredLine) {
	if e.program || (e.line == "" && !e.edited) {
		return // keys for a program other than the shell, or an empty line
	}
	c.mu.Lock()
	if len(c.pending) >= maxPending {
		c.dropped++
		c.mu.Unlock()
		return
	}
	c.pending = append(c.pending, e)
	c.mu.Unlock()
	if !e.whole {
		c.judge(e)
		return
	}
	time.AfterFunc(echoWait, func() { c.judge(e) })
}

// flush judges every line still waiting, when the shell has ended.
func (c *lineCapture) flush() {
	c.mu.Lock()
	waiting := append([]*enteredLine(nil), c.pending...)
	c.mu.Unlock()
	for _, e := range waiting {
		c.judge(e)
	}
}

// judge records a line once, without its text unless the terminal showed it.
func (c *lineCapture) judge(e *enteredLine) {
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
		in := agent.TerminalInput{CallID: c.callID, Line: e.line, Edited: e.edited}
		if !e.echoed() {
			in = agent.TerminalInput{CallID: c.callID, Withheld: withheldEcho}
		}
		c.record(in)
	}
	if dropped > 0 {
		c.record(agent.TerminalInput{CallID: c.callID,
			Withheld: fmt.Sprintf("%d more lines came faster than they could be followed, and are not recorded", dropped)})
	}
}

// echoed reports whether the terminal showed the line as it was typed: the
// whole line, unedited, in its echo. Keys a program read without an Enter
// (read -s -n) stay at the front of the next line, and a line that begins
// with them never matches. When unsure it says no: a line wrongly withheld
// costs the record its text, a line wrongly kept can put a password in it.
func (e *enteredLine) echoed() bool {
	if e.secret || e.edited || e.line == "" || !strings.Contains(plainText(e.echo), e.line) {
		return false
	}
	// A short line needs the terminal to have said it was not reading a password.
	return len(e.line) >= probeMin || e.known
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

func keepTail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return append([]byte(nil), b[len(b)-n:]...)
}
