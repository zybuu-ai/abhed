package abhed

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func forwardingAgent(t *testing.T, on func(Event)) *Agent {
	t.Helper()
	a, err := New(context.Background(), Options{
		Workspace: t.TempDir(),
		Provider:  &Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"}, // never reached
		OnEvent:   on,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func record(t *testing.T, a *Agent, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := a.loop.Recorder.Record(agent.EvPlanUpdated, agent.ActorSystem, agent.Trusted, map[string]int{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
}

func flushWithin(t *testing.T, a *Agent, d time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return a.Flush(ctx)
}

// An event recorded the moment New returns is delivered.
func TestFlushDeliversAnEventRecordedRightAfterNew(t *testing.T) {
	for i := 0; i < 50; i++ {
		var got atomic.Int32
		a := forwardingAgent(t, func(Event) { got.Add(1) })
		record(t, a, 1)
		if err := flushWithin(t, a, 2*time.Second); err != nil || got.Load() != 1 {
			t.Fatalf("iteration %d: flush %v, delivered %d of 1", i, err, got.Load())
		}
	}
}

// A slow OnEvent delays delivery but loses nothing, and Flush returns as soon
// as the last event has been handled.
func TestSlowOnEventLosesNothing(t *testing.T) {
	const n = 700
	var mu sync.Mutex
	var seqs []int64
	var last time.Time
	a := forwardingAgent(t, func(ev Event) {
		time.Sleep(200 * time.Microsecond)
		mu.Lock()
		seqs = append(seqs, ev.Seq)
		last = time.Now()
		mu.Unlock()
	})
	record(t, a, n)
	if err := flushWithin(t, a, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	returned := time.Now()
	mu.Lock()
	defer mu.Unlock()
	if len(seqs) != n {
		t.Fatalf("delivered %d of %d", len(seqs), n)
	}
	for i, s := range seqs {
		if s != int64(i+1) {
			t.Fatalf("event %d delivered as seq %d", i, s)
		}
	}
	if gap := returned.Sub(last); gap > 250*time.Millisecond {
		t.Fatalf("Flush returned %v after the last event", gap)
	}
}

// Flush called from OnEvent cannot be satisfied, and returns its context's
// error rather than hanging.
func TestFlushFromOnEventReturnsItsContextError(t *testing.T) {
	inner := make(chan error, 1)
	var a *Agent
	var once sync.Once
	a = forwardingAgent(t, func(Event) {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			inner <- a.Flush(ctx)
		})
	})
	record(t, a, 2)
	select {
	case err := <-inner:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Flush from OnEvent returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Flush from OnEvent hung")
	}
	if err := flushWithin(t, a, 2*time.Second); err != nil {
		t.Fatalf("Flush after the callback returned: %v", err)
	}
}

// Concurrent writers reach OnEvent in the order the store records them.
func TestOnEventOrderIsTheStoresUnderConcurrentWriters(t *testing.T) {
	var mu sync.Mutex
	var got []string
	a := forwardingAgent(t, func(ev Event) {
		mu.Lock()
		got = append(got, ev.ID)
		mu.Unlock()
	})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record(t, a, 50)
		}()
	}
	wg.Wait()
	if err := flushWithin(t, a, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	stored := a.Events()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(stored) {
		t.Fatalf("delivered %d of %d", len(got), len(stored))
	}
	for i := range stored {
		if got[i] != stored[i].ID {
			t.Fatalf("event %d: OnEvent saw %s, the store holds %s", i, got[i], stored[i].ID)
		}
	}
}

// Once closed, nothing more is delivered, and Flush says so.
func TestFlushAfterCloseIsAnError(t *testing.T) {
	a := forwardingAgent(t, func(Event) {})
	record(t, a, 1)
	if err := flushWithin(t, a, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	a.Close()
	record(t, a, 1)
	if err := flushWithin(t, a, 2*time.Second); !errors.Is(err, errClosed) {
		t.Fatalf("Flush after Close: %v", err)
	}
}

// With no OnEvent nothing is queued, and Flush has nothing to wait for.
func TestFlushWithoutOnEvent(t *testing.T) {
	a := forwardingAgent(t, nil)
	record(t, a, 3)
	if err := flushWithin(t, a, time.Second); err != nil || len(a.fwd.queue) != 0 {
		t.Fatalf("flush %v, queued %d", err, len(a.fwd.queue))
	}
}
