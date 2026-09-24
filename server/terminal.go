package server

import (
	"bytes"
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
// the docs say so (docs/guide/16-workbench.md).

const (
	// echoWait is how long a line's echo has to arrive before it is judged.
	echoWait = 300 * time.Millisecond
	// echoKeep bounds the output kept to find a line's echo in.
	echoKeep = 8 << 10
	// probeLen is how much of the end of a line is looked for in the echo.
	probeLen = 12
)

// enteredLine is one line, at the Enter that submitted it.
type enteredLine struct {
	line   string
	edited bool
	// probe is the end of the last stretch typed without an editing key,
	// which the terminal echoes verbatim when echo is on.
	probe string
	// typedEcho is whether any output arrived while the line was typed.
	typedEcho bool
	alt       bool
	// whole is set when every key of the line is in this chunk, so none of
	// it has reached the shell yet.
	whole bool
	echo  []byte
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
	run   []byte
	esc   int // 0 none, 1 after ESC, 2 in a CSI sequence, 3 after ESC O
	param []byte
	edit  bool
	// started is set by the first key of a line; echo collects the output
	// from then on.
	started   bool
	typedEcho bool
	echo      []byte
	// alt is whether a full-screen program has the alternate screen.
	alt     bool
	pending []*enteredLine
	record  func(agent.TerminalInput)
	callID  string
}

func newLineCapture(callID string, record func(agent.TerminalInput)) *lineCapture {
	return &lineCapture{callID: callID, record: record}
}

// keys splits typed input at each Enter and rebuilds the line each submits.
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
		case b == '\r' || b == '\n':
			e := c.submit()
			e.whole, here = here, false
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
			c.line, c.run, c.edit = c.line[:0], c.run[:0], false
		case b < 0x20:
			c.edited()
		default:
			if len(c.line) < maxManualCommand {
				c.line = append(c.line, b)
				c.run = append(c.run, b)
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

// edited marks the line as changed by a key the capture cannot follow.
func (c *lineCapture) edited() {
	c.edit = true
	c.run = c.run[:0]
}

// submit ends the current line. Called with c.mu held.
func (c *lineCapture) submit() *enteredLine {
	probe := c.run
	if len(probe) > probeLen {
		probe = probe[len(probe)-probeLen:]
		for len(probe) > 0 && !utf8.RuneStart(probe[0]) {
			probe = probe[1:]
		}
	}
	e := &enteredLine{line: string(c.line), edited: c.edit, probe: string(probe),
		typedEcho: c.typedEcho, alt: c.alt, echo: append([]byte(nil), c.echo...)}
	c.line, c.run, c.edit, c.started = c.line[:0], c.run[:0], false, false
	return e
}

// output follows what the terminal wrote: the echo of the line being typed,
// the echo of lines waiting to be judged, and the alternate screen.
func (c *lineCapture) output(chunk []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alt = altScreen(chunk, c.alt)
	if c.started {
		c.typedEcho = true
		c.echo = keepTail(append(c.echo, chunk...), echoKeep)
	}
	for _, p := range c.pending {
		if room := echoKeep - len(p.echo); room > 0 {
			p.echo = append(p.echo, chunk[:min(room, len(chunk))]...)
		}
	}
}

// entered queues a line that reached the shell, to be judged and recorded
// once its echo has had time to arrive.
func (c *lineCapture) entered(e *enteredLine) {
	if e.alt || (e.line == "" && !e.edited) {
		return // a full-screen program's keys, or an empty line
	}
	c.mu.Lock()
	c.pending = append(c.pending, e)
	c.mu.Unlock()
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

// judge records a line once. A line the terminal did not echo is recorded
// without its text: echo off is how a program asks for a password.
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
	c.mu.Unlock()
	if !found {
		return
	}
	in := agent.TerminalInput{CallID: c.callID, Line: e.line, Edited: e.edited}
	if !e.echoed() {
		in = agent.TerminalInput{CallID: c.callID, Withheld: "the terminal did not echo this line"}
	}
	c.record(in)
}

// echoed reports whether the terminal showed the line as it was typed.
func (e *enteredLine) echoed() bool {
	if e.probe == "" {
		return e.typedEcho
	}
	return strings.Contains(plainText(e.echo), e.probe) && (!e.edited || e.typedEcho)
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
