package ui

import (
	"time"
	"unicode"
)

// legacyApproval routes single decision keys to the line-based approver
// (Approver.Prepare), for callers that have not moved to the dialog. Its
// guard is the dialog's: no key counts until the choices have been on screen
// for approvalGuard, a key pressed within approvalGuard of the one before it
// is typing, and a decision key must stand alone for approvalGuard (twice
// that for A) before it answers.
type legacyApproval struct {
	ch       chan rune
	armedAt  time.Time
	heldSent bool
	pending  rune
	pendingN int
}

// approvalGuard is the quiet a decision key needs: after the dialog appears,
// after the previous key, and after itself.
const approvalGuard = 300 * time.Millisecond

// Notices on the approval channel: typing is being kept as steering, or a
// decision key was pressed on a line that already holds text.
const (
	approvalHeld rune = 0
	approvalBusy rune = 1
)

func isDecisionKey(k rune) bool {
	switch k {
	case 'y', 'a', 'A', 'n', 'r':
		return true
	}
	return false
}

func (d *dock) beginLegacy() <-chan rune {
	d.mu.Lock()
	defer d.mu.Unlock()
	ch := make(chan rune, 8)
	d.legacy = &legacyApproval{ch: ch}
	return ch
}

// armLegacy starts the guard: the choices have just been drawn.
func (d *dock) armLegacy() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.legacy != nil && d.legacy.armedAt.IsZero() {
		d.legacy.armedAt = d.now()
	}
}

func (d *dock) endLegacy() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.legacy = nil
}

func (l *legacyApproval) notify(n rune) {
	if n == approvalHeld {
		if l.heldSent {
			return
		}
		l.heldSent = true
	}
	select {
	case l.ch <- n:
	default:
	}
}

// legacyKey applies a key to the waiting approval, reporting whether it was
// consumed; otherwise the key edits the line, as steering.
func (d *dock) legacyKey(k key, at time.Time, gap time.Duration) bool {
	l := d.legacy
	empty := d.buf.empty()
	if held := l.pending; held != 0 {
		l.pending = 0
		if k.code == kNone && k.r == keyEnter && empty {
			l.notify(keyEnter) // a key then Enter: neither answer nor steering
			return true
		}
		d.buf.insert([]rune{held}) // another key followed it: it was typing
		l.notify(approvalHeld)
		empty = false
	}
	quiet := !l.armedAt.IsZero() && at.Sub(l.armedAt) >= approvalGuard && gap >= approvalGuard
	switch {
	case k.code == kPaste:
		l.notify(approvalHeld)
		return false
	case k.code != kNone:
		return true // arrows, Esc and the like never answer
	case k.alt:
		return true
	case k.r == keyEnter && empty:
		l.notify(keyEnter) // never an answer; the choices are shown again
		return true
	case isDecisionKey(k.r) && empty && quiet:
		d.legacyHold(l, k.r)
		return true
	case isDecisionKey(k.r) && quiet:
		l.notify(approvalBusy)
	case k.r != keyCtrlC && k.r != keyEnter && unicode.IsPrint(k.r):
		l.notify(approvalHeld)
	}
	return false
}

func (d *dock) legacyHold(l *legacyApproval, k rune) {
	l.pending = k
	l.pendingN++
	n := l.pendingN
	wait := approvalGuard
	if k == 'A' {
		wait = 2 * approvalGuard // it allows for the rest of the session
	}
	d.after(wait, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.legacy != l || l.pendingN != n || l.pending == 0 {
			return
		}
		select {
		case l.ch <- l.pending:
		default:
		}
		l.pending = 0
	})
}
