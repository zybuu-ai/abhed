package model

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// A provider's request bound is shared by every adapter built from the same
// spec in this process: a session's, its subagents' on a named model, the
// editor's completions. One bound per adapter let each of them send its own
// request to a server that serves one at a time, and the queued ones stalled.

// slotPool bounds the requests in flight to one provider.
type slotPool struct {
	slots   chan struct{}
	waiting atomic.Int32
}

var (
	poolsMu sync.Mutex
	pools   = map[string]*slotPool{}
)

// poolFor is the shared pool for a provider identity and bound.
func poolFor(typ, baseURL, model string, n int) *slotPool {
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%d", typ, baseURL, model, n)
	poolsMu.Lock()
	defer poolsMu.Unlock()
	p := pools[key]
	if p == nil {
		p = &slotPool{slots: make(chan struct{}, n)}
		pools[key] = p
	}
	return p
}

// SlotWait is a request held back because its provider's slots are taken.
type SlotWait struct {
	// Ahead is how many requests were waiting before this one.
	Ahead int
	// Waited is how long it waited, once it has a slot.
	Waited time.Duration
}

type slotWaitKey struct{}

// WithSlotWait has f told when a request on ctx waits for a slot (done
// false) and when it gets one (done true). A request that does not wait is
// not reported.
func WithSlotWait(ctx context.Context, f func(w SlotWait, done bool)) context.Context {
	return context.WithValue(ctx, slotWaitKey{}, f)
}

// SlotWaitOf is what WithSlotWait put on ctx, or nil: an adapter that holds
// requests back by other means reports its waits through it.
func SlotWaitOf(ctx context.Context) func(w SlotWait, done bool) {
	f, _ := ctx.Value(slotWaitKey{}).(func(SlotWait, bool))
	return f
}

// acquire waits for a slot and returns what frees it; it gives up when ctx
// ends. A nil pool sets no bound.
func (p *slotPool) acquire(ctx context.Context) (func(), error) {
	if p == nil {
		return func() {}, nil
	}
	select {
	case p.slots <- struct{}{}:
	default:
		tell := SlotWaitOf(ctx)
		ahead := int(p.waiting.Add(1)) - 1
		start := time.Now()
		if tell != nil {
			tell(SlotWait{Ahead: ahead}, false)
		}
		select {
		case p.slots <- struct{}{}:
			p.waiting.Add(-1)
		case <-ctx.Done():
			p.waiting.Add(-1)
			return nil, ctx.Err()
		}
		if tell != nil {
			tell(SlotWait{Ahead: ahead, Waited: time.Since(start)}, true)
		}
	}
	var once sync.Once
	return func() { once.Do(func() { <-p.slots }) }, nil
}
