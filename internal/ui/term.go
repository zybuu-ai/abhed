package ui

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

// Attention is what the terminal is told about the session, so a person who
// runs the agent in another tab knows when it wants them. Only fixed, generic
// words are ever sent: never a command, a path or anything the model wrote.
type Attention struct {
	// Title sets the window title (OSC 0) to the session's state.
	Title bool
	// Notify is how a waiting approval or a finished turn is signalled while
	// the terminal is unfocused: "bel", "osc9", "off", or "auto" (OSC 9 where
	// the terminal is known to show it, else BEL).
	Notify string
}

// AttnState is the session's state as the title and notifications tell it.
type AttnState int

const (
	AttnReady AttnState = iota
	AttnWorking
	AttnApproval
)

func (s AttnState) title() string {
	switch s {
	case AttnWorking:
		return "abhed · working"
	case AttnApproval:
		return "abhed · approval needed"
	}
	return "abhed · ready"
}

// attention is the dock's side of it: the settings, whether the terminal has
// said it is focused (0 not said, 1 focused, -1 not), and what was last set.
type attention struct {
	cfg   Attention
	on    bool
	focus int
	state AttnState
	// pushed is read without the lock by a panic's restore.
	pushed atomic.Bool
}

// SetAttention applies the settings. The terminal's own title is saved on its
// title stack first and put back when the reader closes.
func (l *LineReader) SetAttention(a Attention) {
	if !l.raw {
		return
	}
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	d := l.d
	d.attn.cfg, d.attn.on = a, true
	if a.Title && !d.stopped && d.attn.pushed.CompareAndSwap(false, true) {
		d.scr.raw("\x1b[22;0t")
		d.scr.raw(osc0(d.attn.state.title()))
	}
}

// Attend tells the terminal the session's state: the title follows it, and
// an unfocused terminal is signalled when an approval starts waiting and when
// a turn ends.
func (l *LineReader) Attend(st AttnState) {
	if !l.raw {
		return
	}
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	l.d.attend(st)
}

// attending is the state last told, so a dialog can put it back.
func (l *LineReader) attending() AttnState {
	if !l.raw {
		return AttnReady
	}
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	return l.d.attn.state
}

func (d *dock) attend(st AttnState) {
	a := &d.attn
	if !a.on || d.stopped {
		return
	}
	prev := a.state
	a.state = st
	if a.cfg.Title && st != prev {
		d.scr.raw(osc0(st.title()))
	}
	var msg string
	switch {
	case st == AttnApproval && prev != AttnApproval:
		msg = "abhed: approval waiting"
	case st == AttnReady && prev == AttnWorking:
		msg = "abhed: turn finished"
	}
	// Only a terminal that said it lost focus is signalled: one that never
	// reports focus is assumed to be watched.
	if msg != "" && a.focus < 0 {
		if seq := notifySeq(a.cfg.Notify, msg); seq != "" {
			d.scr.raw(seq)
		}
	}
}

// focused records a focus report (CSI I or CSI O).
func (d *dock) focused(in bool) {
	if in {
		d.attn.focus = 1
	} else {
		d.attn.focus = -1
	}
}

// osc0 sets the window and tab title. The text is ours, but is cleaned all
// the same, so no change to the words can end the sequence early.
func osc0(text string) string { return "\x1b]0;" + oscText(text) + st }

// st ends an OSC. ST rather than BEL, so a bell on the wire is only ever
// the notification itself.
const st = "\x1b\\"

// oscText keeps printable text only, with no BEL, ESC or C1 to close an OSC.
func oscText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

// notifySeq is the signal for mode, or "" for none.
func notifySeq(mode, msg string) string {
	switch mode {
	case "off":
		return ""
	case "bel":
		return "\a"
	case "osc9":
		return "\x1b]9;" + oscText(msg) + st
	}
	if osc9Terminal() {
		return "\x1b]9;" + oscText(msg) + st
	}
	return "\a"
}

// osc9Terminal reports a terminal known to turn OSC 9 into a desktop
// notification. Under tmux or screen the sequence would not reach it.
func osc9Terminal() bool {
	if os.Getenv("TMUX") != "" || strings.HasPrefix(os.Getenv("TERM"), "screen") {
		return false
	}
	switch os.Getenv("TERM_PROGRAM") {
	case "iTerm.app", "WezTerm", "ghostty":
		return true
	}
	return false
}

// MaxCopyBytes is the most /copy sends: several terminals drop a longer OSC 52.
const MaxCopyBytes = 64 << 10

// Copy puts text on the clipboard through the terminal (OSC 52). It only
// ever runs on the person's own command. The terminal may refuse it, and
// says nothing either way.
func (l *LineReader) Copy(text string) error {
	if !l.raw {
		return fmt.Errorf("copying needs a terminal")
	}
	if len(text) > MaxCopyBytes {
		return fmt.Errorf("the reply is %d KB; the terminal clipboard takes at most %d KB", (len(text)+1023)>>10, MaxCopyBytes>>10)
	}
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	if l.d.stopped {
		return fmt.Errorf("the terminal is closed")
	}
	l.d.scr.raw("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + st)
	return nil
}

// restoreTitle puts back the title saved by SetAttention. The dock's lock is held.
func (d *dock) restoreTitle() {
	if d.attn.pushed.Swap(false) {
		d.scr.raw("\x1b[23;0t")
	}
}
