package server

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"
)

// The heartbeat goroutine must end with its context, or every finished turn
// leaks one for the life of the process.
func TestHeartbeatGoroutineDoesNotLeak(t *testing.T) {
	s := &Server{
		store:   &countingRouter{},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		running: map[string]*liveSession{},
		opts:    Options{NodeID: "n"},
	}
	settle := func() int {
		for i := 0; i < 50; i++ {
			runtime.Gosched()
			time.Sleep(10 * time.Millisecond)
		}
		return runtime.NumGoroutine()
	}
	before := settle()
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		_ = s.heartbeatNodeEvery(ctx, "s", 5*time.Millisecond)
		cancel() // the run ending is what stops it
	}
	after := settle()
	if after > before+5 {
		t.Fatalf("goroutines %d -> %d after 50 heartbeats: they are not ending", before, after)
	}
	t.Logf("goroutines %d -> %d across 50 heartbeats", before, after)
}
