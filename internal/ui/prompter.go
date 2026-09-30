package ui

import (
	"context"
	"strings"
	"sync"
)

// Prompter routes a typed line to a pending approval.
//
// The interactive loop runs one reader on stdin (the line editor) and consumes
// its lines in a select loop. An approval that read stdin itself would fight
// that reader for bytes. Instead the approver parks on Await, the reader loop
// hands the next line to Deliver, and there is still only one reader.
type Prompter struct {
	mu      sync.Mutex
	waiting chan string
	stop    chan struct{}
	once    sync.Once
}

// NewPrompter returns a Prompter ready to route approval input.
func NewPrompter() *Prompter { return &Prompter{stop: make(chan struct{})} }

// Await blocks until a line is delivered, the context is cancelled, or input
// ends. ok is false in the latter two cases.
func (p *Prompter) Await(ctx context.Context) (string, bool) {
	ch := make(chan string, 1)
	p.mu.Lock()
	p.waiting = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.waiting = nil
		p.mu.Unlock()
	}()
	select {
	case s := <-ch:
		return s, true
	case <-ctx.Done():
		return "", false
	case <-p.stop:
		return "", false
	}
}

// Deliver hands a line to a pending Await. It reports whether one was waiting,
// so the caller knows to consume the line rather than treat it as steering.
func (p *Prompter) Deliver(line string) bool {
	p.mu.Lock()
	ch := p.waiting
	p.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- line:
		return true
	default:
		return false
	}
}

// Waiting reports whether an approval is waiting for a line.
func (p *Prompter) Waiting() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waiting != nil
}

// Decision reports whether a line is exactly one of an approval's keys, and
// so may answer one. Any other line is never taken as an answer by position.
func Decision(line string) bool {
	switch strings.TrimSpace(line) {
	case "a", "y", "r", "n", "A":
		return true
	}
	return false
}

// Close reports that input has ended, so any current or future Await refuses
// rather than blocking forever.
func (p *Prompter) Close() { p.once.Do(func() { close(p.stop) }) }
