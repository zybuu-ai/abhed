package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func drainServer(budget time.Duration) *Server {
	return &Server{
		running: map[string]*liveSession{},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		opts:    Options{DrainTimeout: budget},
	}
}

// A turn that finishes inside the budget is never cancelled: that is the
// whole point of draining rather than ending everything at once.
func TestDrainWaitsForARunningTurn(t *testing.T) {
	s := drainServer(5 * time.Second)
	var cause error
	live := &liveSession{
		ID: "s-1", State: "running",
		cancelCause: func(err error) { cause = err },
	}
	s.running["s-1"] = live

	go func() {
		time.Sleep(200 * time.Millisecond)
		live.mu.Lock()
		live.State = "done"
		live.mu.Unlock()
	}()

	start := time.Now()
	s.drain()
	if took := time.Since(start); took < 150*time.Millisecond {
		t.Fatalf("drain returned after %v — it did not wait for the turn", took)
	}
	if cause != nil {
		t.Fatalf("a turn that finished in time was cancelled with %v", cause)
	}
}

// A turn that overruns the budget is ended with a stated cause, so it records
// as a shutdown and can be resumed elsewhere rather than looking like a user
// interrupt.
func TestDrainCancelsWhatOverrunsTheBudget(t *testing.T) {
	s := drainServer(150 * time.Millisecond)
	var cause error
	s.running["s-2"] = &liveSession{
		ID: "s-2", State: "running",
		cancelCause: func(err error) { cause = err },
	}

	s.drain()
	if !errors.Is(cause, agent.ErrShutdown) {
		t.Fatalf("cancel cause = %v, want ErrShutdown so the turn records as a shutdown", cause)
	}
}

// Without a budget the old behaviour stands: end everything at once. A single
// node with no balancer in front of it has nowhere to drain to.
func TestDrainWithoutBudgetEndsTurnsAtOnce(t *testing.T) {
	s := drainServer(0)
	var cause error
	s.running["s-3"] = &liveSession{
		ID: "s-3", State: "running",
		cancelCause: func(err error) { cause = err },
	}

	start := time.Now()
	s.drain()
	if took := time.Since(start); took > time.Second {
		t.Fatalf("drain with no budget took %v — it should not wait", took)
	}
	if !errors.Is(cause, agent.ErrShutdown) {
		t.Fatalf("cancel cause = %v, want ErrShutdown", cause)
	}
}

// An idle session holds no turn. Waiting on one would spend the whole budget
// for nothing, and every resumable session would block every deploy.
func TestDrainIgnoresIdleSessions(t *testing.T) {
	s := drainServer(5 * time.Second)
	s.running["idle"] = &liveSession{ID: "idle", State: "done"}

	start := time.Now()
	s.drain()
	if took := time.Since(start); took > time.Second {
		t.Fatalf("drain waited %v on an idle session", took)
	}
}

// Draining refuses a resume, so a balancer sends the request to a node that
// can take it rather than this one starting work it is about to abandon.
func TestDrainingRefusesAResume(t *testing.T) {
	s := drainServer(time.Second)
	s.draining.Store(true)

	_, err := s.resumeSession(context.Background(), "s-4", "prompt", "user", "tenant")
	if !errors.Is(err, errDraining) {
		t.Fatalf("resume during drain = %v, want errDraining", err)
	}
}

// The flag only goes up once drain starts: a healthy node takes work.
func TestNotDrainingUntilShutdown(t *testing.T) {
	s := drainServer(time.Second)
	if s.draining.Load() {
		t.Fatal("a server was draining before shutdown began")
	}
	s.drain()
	if !s.draining.Load() {
		t.Fatal("drain did not mark the server as draining")
	}
}
