package abhed

import (
	"context"
	"sync"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// forwarder hands OnEvent every event the agent records, in the order the
// store records them, from a goroutine of its own. It queues on Append rather than
// reading the store's subscription, which drops what a slow reader has not
// taken; so a slow OnEvent delays delivery but never loses an event.
type forwarder struct {
	*agent.MemStore
	mu        sync.Mutex
	cond      *sync.Cond
	queue     []agent.Event
	appended  int // events written
	delivered int // events OnEvent has returned from
	closed    bool
	// on is false when there is no OnEvent, and nothing is queued.
	on bool
}

func newForwarder(store *agent.MemStore, on bool) *forwarder {
	f := &forwarder{MemStore: store, on: on}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// Append writes the event and queues it for OnEvent under one lock, so
// concurrent writers reach OnEvent in the order the store took them.
func (f *forwarder) Append(ev agent.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.MemStore.Append(ev); err != nil {
		return err
	}
	if !f.on || f.closed {
		return nil
	}
	f.queue = append(f.queue, ev)
	f.appended++
	f.cond.Broadcast()
	return nil
}

// run calls on for each queued event until close, then returns.
func (f *forwarder) run(on func(agent.Event)) {
	for {
		f.mu.Lock()
		for len(f.queue) == 0 && !f.closed {
			f.cond.Wait()
		}
		if f.closed {
			f.mu.Unlock()
			return
		}
		ev := f.queue[0]
		f.queue = f.queue[1:]
		f.mu.Unlock()

		on(ev)

		f.mu.Lock()
		f.delivered++
		f.cond.Broadcast()
		f.mu.Unlock()
	}
}

// flush waits until on has returned for every event written before the call.
func (f *forwarder) flush(ctx context.Context) error {
	wake := context.AfterFunc(ctx, func() {
		f.mu.Lock()
		f.cond.Broadcast()
		f.mu.Unlock()
	})
	defer wake()
	f.mu.Lock()
	defer f.mu.Unlock()
	target := f.appended
	for {
		if f.closed {
			return errClosed
		}
		if f.delivered >= target {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		f.cond.Wait()
	}
}

func (f *forwarder) close() {
	f.mu.Lock()
	f.closed = true
	f.cond.Broadcast()
	f.mu.Unlock()
}
