package app

import (
	"context"
	"errors"
	"sync"
)

// Inline completions go before next-edit suggestions on a model server they
// share. A local server often answers one request at a time, so a next-edit
// call (a whole window rewritten, hundreds of tokens) in front of a
// completion made the ghost text wait seconds for it. While a completion is
// asked for on a server, next-edit calls to it wait, and one already sent is
// cancelled; a rename finished from the file needs no model and never waits.

// errCompletionFirst is the cause a next-edit call is cancelled with when a
// completion takes the server.
var errCompletionFirst = errors.New("a completion went first")

// editorPool is one connection's model calls by server.
type editorPool struct {
	mu sync.Mutex
	// busy counts completions asked for and not yet answered, by server;
	// idle is closed when that falls back to zero.
	busy map[string]int
	idle map[string]chan struct{}
	// next are the next-edit calls in flight, by server.
	next map[string]map[uint64]context.CancelCauseFunc
	n    uint64
}

// completing marks a completion asked for on key, from its arrival to its
// answer, and cancels the next-edit calls in flight there; done ends it.
func (p *editorPool) completing(key string) (done func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy == nil {
		p.busy, p.idle = map[string]int{}, map[string]chan struct{}{}
	}
	p.busy[key]++
	if p.idle[key] == nil {
		p.idle[key] = make(chan struct{})
	}
	for id, cancel := range p.next[key] {
		cancel(errCompletionFirst)
		delete(p.next[key], id)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.busy[key]--; p.busy[key] <= 0 {
				delete(p.busy, key)
				if ch := p.idle[key]; ch != nil {
					close(ch)
					delete(p.idle, key)
				}
			}
		})
	}
}

// nextEditTurn waits until no completion is asked for on key ("" waits for
// nothing: a server that does not queue), then gives a
// next-edit call a context a completion arriving later cancels (its cause
// errCompletionFirst); done releases it. It fails when ctx ends first.
func (p *editorPool) nextEditTurn(ctx context.Context, key string) (context.Context, func(), error) {
	if key == "" {
		cctx, cancel := context.WithCancelCause(ctx)
		return cctx, func() { cancel(nil) }, nil
	}
	for {
		p.mu.Lock()
		ch := p.idle[key]
		if ch == nil {
			if p.next == nil {
				p.next = map[string]map[uint64]context.CancelCauseFunc{}
			}
			if p.next[key] == nil {
				p.next[key] = map[uint64]context.CancelCauseFunc{}
			}
			p.n++
			id := p.n
			cctx, cancel := context.WithCancelCause(ctx)
			p.next[key][id] = cancel
			p.mu.Unlock()
			return cctx, func() {
				p.mu.Lock()
				delete(p.next[key], id)
				p.mu.Unlock()
				cancel(nil)
			}, nil
		}
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}
