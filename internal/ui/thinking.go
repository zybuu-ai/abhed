package ui

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Thinking is the indicator shown while the model works.
//
// A cold 26B model can take thirty seconds to its first token. Without a
// moving indicator that wait is indistinguishable from a hang, and the honest
// fix is not a faster model but telling the user the process is alive and how
// long it has been.
//
// The verbs are Abhed's own and describe what a HARNESS does — weighing,
// scoping, bounding — rather than what a chatbot does. They change on a slow
// cycle so a long wait does not read as a frozen frame, and the elapsed
// seconds appear once the wait is long enough to wonder about.
type Thinking struct {
	w   io.Writer
	s   Style
	mu  sync.Mutex
	on  bool
	end chan struct{}
	dn  chan struct{}
}

// Braille frames: one cell wide in every terminal, and they rotate rather
// than flash, which is calmer next to streaming text.
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// The vocabulary. Deliberately about the harness's work, not the model's
// personality: this is infrastructure, and cute verbs age badly on the
// hundredth run.
var thinkingVerbs = []string{
	"Thinking", "Reasoning", "Weighing", "Planning", "Considering",
	"Scoping", "Tracing", "Reading", "Composing", "Deliberating",
	"Working", "Assembling", "Checking", "Resolving", "Drafting",
}

func NewThinking(w io.Writer, s Style) *Thinking { return &Thinking{w: w, s: s} }

// Start begins animating. Calling it twice is a no-op, so a caller does not
// have to track whether a turn already started one.
func (t *Thinking) Start() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.on {
		return
	}
	t.on = true
	t.end = make(chan struct{})
	t.dn = make(chan struct{})

	go func(end, done chan struct{}) {
		defer close(done)
		started := time.Now()
		// Walk the list rather than sampling it. Random choice needed a PRNG,
		// which gosec flags as a weak generator — a fair complaint to raise
		// even when the stake is only which word appears, because a reader
		// cannot tell from the call site that the stake is low. Walking also
		// behaves better: sampling repeats itself on a long wait, and the same
		// verb twice running reads as a stuck frame.
		vi := int(time.Now().UnixNano() % int64(len(thinkingVerbs)))
		verb := thinkingVerbs[vi]
		tick := time.NewTicker(90 * time.Millisecond)
		defer tick.Stop()
		i := 0
		for {
			select {
			case <-end:
				return
			case <-tick.C:
				i++
				// A new verb every ~4s: enough to show progress, slow enough
				// not to jitter.
				if i%44 == 0 {
					vi = (vi + 1) % len(thinkingVerbs)
					verb = thinkingVerbs[vi]
				}
				frame := spinFrames[i%len(spinFrames)]
				el := ""
				if d := time.Since(started); d > 3*time.Second {
					el = t.s.Dim(fmt.Sprintf("  %ds", int(d.Seconds())))
				}
				// \r and clear-to-end: one line, rewritten in place, so the
				// transcript above is never disturbed.
				fmt.Fprintf(t.w, "\r\033[2K%s %s%s",
					t.s.Accent(frame), t.s.Dim(verb+"…"), el)
			}
		}
	}(t.end, t.dn)
}

// Stop halts the animation and erases the line, leaving the cursor where the
// next output should begin. Safe to call when not running.
func (t *Thinking) Stop() {
	t.mu.Lock()
	if !t.on {
		t.mu.Unlock()
		return
	}
	t.on = false
	end, done := t.end, t.dn
	t.mu.Unlock()

	close(end)
	<-done // wait, or a final frame can be written after the erase
	fmt.Fprint(t.w, "\r\033[2K")
}

// Active reports whether the indicator is running, so a renderer can stop it
// before writing and restart it afterwards.
func (t *Thinking) Active() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.on
}

// Verbs is exported for tests that assert the vocabulary stays harness-shaped.
func Verbs() []string { return append([]string(nil), thinkingVerbs...) }
