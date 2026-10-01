package termline

import (
	"strings"
	"sync"
)

// promptKeep bounds the output kept to tell a shell's prompt is back.
const promptKeep = 4 << 10

// Prompt follows whether a shell is back at its prompt after the last line it
// was given. A line entered before then is typed ahead: what reads it may turn
// echo off after it arrived, so its text is withheld.
type Prompt struct {
	mu    sync.Mutex
	at    bool
	since []byte
}

// NewPrompt follows a shell that has not shown its first prompt yet.
func NewPrompt() *Prompt { return &Prompt{since: []byte("\n")} }

// Output notes what the shell wrote, with the terminal as asked just after:
// the prompt is back when the shell is in front out of canonical mode, and
// text stands on a line begun after the given line's newline.
func (p *Prompt) Output(chunk []byte, shellFront, canonical bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.since = KeepTail(append(p.since, chunk...), promptKeep)
	if !shellFront || canonical {
		return
	}
	text := PlainText(p.since)
	if i := strings.LastIndexByte(text, '\n'); i >= 0 && strings.TrimSpace(text[i+1:]) != "" {
		p.at = true
	}
}

// OutputUnasked notes what the shell wrote where the terminal cannot be
// asked (a container's, which the engine's CLI holds). Only Abhed's own
// prompt, which ends in "$ " or "# ", counts as the shell back at it: a
// password prompt or a progress line does not, so a line typed then is taken
// as typed ahead and its text withheld. A prompt changed from Abhed's makes
// every line withheld, which errs the safe way.
func (p *Prompt) OutputUnasked(chunk []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.since = KeepTail(append(p.since, chunk...), promptKeep)
	text := PlainText(p.since)
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		last := text[i+1:]
		// Once back, it stays back until the next line: the keys typed at
		// the prompt are echoed after it.
		p.at = p.at || strings.HasSuffix(last, "$ ") || strings.HasSuffix(last, "# ")
	}
}

// Gave notes that the shell was handed a line.
func (p *Prompt) Gave() {
	p.mu.Lock()
	p.at, p.since = false, nil
	p.mu.Unlock()
}

// At reports whether the shell is at its prompt.
func (p *Prompt) At() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.at
}
