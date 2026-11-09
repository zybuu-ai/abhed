package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// oneAtATime serves as Ollama does by default: one request at a time, the
// rest queued with nothing sent. The request being served streams a byte
// every 20ms, so it never stalls, until hold is closed; then it ends.
func oneAtATime(t *testing.T, hold <-chan struct{}) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok \"}}]}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-hold:
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				return
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// concurrently makes one call on each adapter at once, releases the server
// after a while, and returns the errors the calls ended with.
func concurrently(ctx context.Context, as []Adapter, hold chan struct{}, after time.Duration) []error {
	errs := make([]error, len(as))
	var wg sync.WaitGroup
	for i, a := range as {
		wg.Add(1)
		go func(i int, a Adapter) {
			defer wg.Done()
			ch, err := a.Complete(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
			if err != nil {
				errs[i] = err
				return
			}
			for c := range ch {
				if c.Type == ChunkError {
					errs[i] = c.Err
				}
			}
		}(i, a)
	}
	time.AfterFunc(after, func() { close(hold) })
	wg.Wait()
	return errs
}

func adapters(t *testing.T, spec Spec, n int) []Adapter {
	t.Helper()
	out := make([]Adapter, n)
	for i := range out {
		a, err := New(spec)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = a
	}
	return out
}

// The stub queues: with no bound, a second call at once is silent for as
// long as the first is served, and the stall timeout ends it.
func TestUnboundedCallsStallOnAOneAtATimeServer(t *testing.T) {
	hold := make(chan struct{})
	spec := Spec{Type: "openai-compatible", BaseURL: oneAtATime(t, hold).URL + "/v1", Model: "m",
		Timeouts: Timeouts{Stall: 500 * time.Millisecond}}
	stalls := 0
	for _, err := range concurrently(context.Background(), adapters(t, spec, 2), hold, 2*time.Second) {
		if err != nil {
			stalled(t, err, true)
			stalls++
		}
	}
	if stalls != 1 {
		t.Fatalf("%d of 2 calls stalled, want the queued one", stalls)
	}
}

// Four adapters built from one Ollama spec, as a session, two subagents on
// a named model and the editor's completions build them, share its one
// slot: the requests past it wait here, before their timeouts start, and
// every call ends well. Each waiter is told it waits, then that it is through.
func TestAdaptersFromOneSpecShareItsSlots(t *testing.T) {
	hold := make(chan struct{})
	spec := Spec{Type: "ollama", BaseURL: oneAtATime(t, hold).URL + "/v1", Model: "m",
		Timeouts: Timeouts{Stall: 500 * time.Millisecond}}
	var waits, through atomic.Int32
	ctx := WithSlotWait(context.Background(), func(w SlotWait, done bool) {
		if done {
			through.Add(1)
		} else {
			waits.Add(1)
		}
	})
	for i, err := range concurrently(ctx, adapters(t, spec, 4), hold, 1500*time.Millisecond) {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
	if waits.Load() != 3 || through.Load() != 3 {
		t.Fatalf("%d waits and %d through, want 3 of each", waits.Load(), through.Load())
	}

	// max_concurrent_requests sets the bound for any OpenAI-compatible type.
	hold = make(chan struct{})
	spec = Spec{Type: "openai-compatible", BaseURL: oneAtATime(t, hold).URL + "/v1", Model: "m",
		Timeouts: Timeouts{Stall: 500 * time.Millisecond}, MaxConcurrent: 1}
	for i, err := range concurrently(context.Background(), adapters(t, spec, 3), hold, 1500*time.Millisecond) {
		if err != nil {
			t.Errorf("call %d with max_concurrent_requests 1: %v", i, err)
		}
	}
}

// A call that gives up while waiting for a slot ends with its context's
// error and takes no slot.
func TestWaitingForASlotEndsWithTheCall(t *testing.T) {
	a, err := New(Spec{Type: "ollama", BaseURL: "http://127.0.0.1:1/v1", Model: "wait-cancel"})
	if err != nil {
		t.Fatal(err)
	}
	c := a.(*OpenAICompatible)
	held, err := c.slots.acquire(context.Background()) // the one slot is held
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Complete(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != context.DeadlineExceeded {
		t.Fatalf("got %v, want the context's deadline", err)
	}
	if c.slots.waiting() != 0 {
		t.Fatalf("%d still waiting", c.slots.waiting())
	}
	held()
	if next, err := c.slots.acquire(context.Background()); err != nil {
		t.Fatalf("the slot was not freed: %v", err)
	} else {
		next()
	}
}

// Providers of different types on one server and model share its pool, with
// the smallest bound any of them gives; spellings of the URL that differ
// only by case, a default port or a trailing slash are one server.
func TestProvidersOnOneServerShareTheSmallestBound(t *testing.T) {
	hold := make(chan struct{})
	base := oneAtATime(t, hold).URL
	var as []Adapter
	for _, sp := range []Spec{
		{Type: "ollama", BaseURL: base + "/v1", Model: "m"},
		{Type: "openai-compatible", BaseURL: base + "/v1/", Model: "m", MaxConcurrent: 1},
		{Type: "openai-compatible", BaseURL: strings.ToUpper(base[:4]) + base[4:] + "/v1", Model: "m"},
	} {
		sp.Timeouts = Timeouts{Stall: 500 * time.Millisecond}
		a, err := New(sp)
		if err != nil {
			t.Fatal(err)
		}
		as = append(as, a)
	}
	for i, err := range concurrently(context.Background(), as, hold, 1500*time.Millisecond) {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
	if endpointKey("http://Host:80/v1/", "m") != endpointKey("http://host/v1", "m") {
		t.Error("a default port or a trailing slash makes another server")
	}
	if p := poolFor("http://smallest.test/v1", "m", 4); poolFor("http://smallest.test/v1", "m", 2).limit != 2 || p.limit != 2 ||
		poolFor("http://smallest.test/v1", "m", 0).limit != 2 {
		t.Error("the smallest bound did not win")
	}
}

// The bound is refused where it would be ignored, and when negative.
func TestMaxConcurrentRefusedWhereIgnored(t *testing.T) {
	if _, err := New(Spec{Type: "ollama", BaseURL: "http://x/v1", Model: "m", MaxConcurrent: -1}); err == nil {
		t.Fatal("a negative max_concurrent_requests was accepted")
	}
	_, err := New(Spec{Type: "anthropic", BaseURL: "http://x", Model: "m", APIKey: "k", MaxConcurrent: 2})
	if err == nil || !strings.Contains(err.Error(), "OpenAI-compatible") {
		t.Fatalf("got %v, want a refusal naming where it applies", err)
	}
}
