package model

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A server's request bound is shared by every adapter that reaches the same
// model there in this process: a session's, its subagents' on a named model,
// the editor's completions, and providers of different types that name the
// same endpoint. One bound per adapter let each of them send its own request
// to a server that serves one at a time, and the queued ones stalled.

// slotPool bounds the requests in flight to one model on one server. A limit
// of 0 sets no bound; the smallest bound any provider gives it wins.
type slotPool struct {
	mu      sync.Mutex
	limit   int
	inUse   int
	waiters []chan struct{} // oldest first; each is closed when it is given a slot
}

var (
	poolsMu sync.Mutex
	pools   = map[string]*slotPool{}
)

// endpointKey is a base URL and model with case, default ports and trailing
// slashes taken out, so two spellings of one server share a pool.
func endpointKey(baseURL, model string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return strings.TrimRight(strings.ToLower(baseURL), "/") + "\x00" + model
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return strings.ToLower(u.Scheme) + "://" + host + strings.TrimRight(u.Path, "/") + "\x00" + model
}

// poolFor is the shared pool for a model on a server; n above 0 bounds it,
// lowering any larger bound it had. The bound never rises within the
// process: a configuration that raises it takes effect at the next start.
func poolFor(baseURL, model string, n int) *slotPool {
	key := endpointKey(baseURL, model)
	poolsMu.Lock()
	defer poolsMu.Unlock()
	p := pools[key]
	if p == nil {
		p = &slotPool{}
		pools[key] = p
	}
	p.mu.Lock()
	if n > 0 && (p.limit == 0 || n < p.limit) {
		p.limit = n
	}
	p.mu.Unlock()
	return p
}

// SlotWait is a request held back because its server's slots are taken.
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
// ends. A nil pool, or one with no bound, lets every request through.
func (p *slotPool) acquire(ctx context.Context) (func(), error) {
	if p == nil {
		return func() {}, nil
	}
	p.mu.Lock()
	if p.limit == 0 || (p.inUse < p.limit && len(p.waiters) == 0) {
		p.inUse++
		p.mu.Unlock()
		return p.releaser(), nil
	}
	turn := make(chan struct{})
	ahead := len(p.waiters)
	p.waiters = append(p.waiters, turn)
	p.mu.Unlock()

	tell := SlotWaitOf(ctx)
	start := time.Now()
	if tell != nil {
		tell(SlotWait{Ahead: ahead}, false)
	}
	select {
	case <-turn:
	case <-ctx.Done():
		p.mu.Lock()
		given := true
		for i, w := range p.waiters {
			if w == turn {
				p.waiters = append(p.waiters[:i:i], p.waiters[i+1:]...)
				given = false
				break
			}
		}
		p.mu.Unlock()
		if given { // the slot came as it gave up: hand it on
			p.release()
		}
		return nil, ctx.Err()
	}
	if tell != nil {
		tell(SlotWait{Ahead: ahead, Waited: time.Since(start)}, true)
	}
	return p.releaser(), nil
}

// releaser frees one slot, once however often it is called.
func (p *slotPool) releaser() func() {
	var once sync.Once
	return func() { once.Do(p.release) }
}

// release frees a slot, giving it to the oldest waiter if there is one.
func (p *slotPool) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.waiters) > 0 && (p.limit == 0 || p.inUse <= p.limit) {
		next := p.waiters[0]
		p.waiters = p.waiters[1:]
		close(next) // the slot passes to it; inUse is unchanged
		return
	}
	p.inUse--
}

// waiting is how many requests wait for a slot, for tests.
func (p *slotPool) waiting() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.waiters)
}

// HonoursMaxConcurrent reports whether a provider type takes
// max_concurrent_requests: one registered as speaking the OpenAI-compatible
// protocol.
func HonoursMaxConcurrent(typ string) bool { return providers[typ].openAI }
