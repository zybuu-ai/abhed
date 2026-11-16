package server

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// A hook that never answers, ignores its context or panics stops the run
// as unavailable within the wait; an interrupted run is not held up.
func TestAllowanceWaitPanicAndInterrupt(t *testing.T) {
	was := allowanceWait
	allowanceWait = 100 * time.Millisecond
	t.Cleanup(func() { allowanceWait = was })
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	ask := func(f func(context.Context, AllowanceRequest) (Allowance, error)) func(context.Context) (bool, string, time.Duration) {
		s := &Server{opts: Options{SessionAllowance: f}, log: slog.New(slog.DiscardHandler)}
		check := s.allowance(StartSpec{Tenant: "default", User: "bob"}, "s1")
		return func(ctx context.Context) (bool, string, time.Duration) {
			start := time.Now()
			a := check(ctx)
			return a.Stop, a.Scope, time.Since(start)
		}
	}
	deaf := ask(func(context.Context, AllowanceRequest) (Allowance, error) { <-block; return Allowance{}, nil })
	if stop, scope, took := deaf(context.Background()); !stop || scope != "unavailable" || took > 2*time.Second {
		t.Fatalf("a hook that ignores its deadline: stop %v scope %q after %v", stop, scope, took)
	}
	panics := ask(func(context.Context, AllowanceRequest) (Allowance, error) { panic("boom") })
	if stop, scope, _ := panics(context.Background()); !stop || scope != "unavailable" {
		t.Fatalf("a panic: stop %v scope %q", stop, scope)
	}
	// A hook's own Canceled is no interrupt while the run's context lives.
	cancelled := ask(func(context.Context, AllowanceRequest) (Allowance, error) { return Allowance{}, context.Canceled })
	if stop, scope, _ := cancelled(context.Background()); !stop || scope != "unavailable" {
		t.Fatalf("a hook returning Canceled: stop %v scope %q", stop, scope)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	slow := ask(func(ctx context.Context, _ AllowanceRequest) (Allowance, error) {
		<-ctx.Done()
		return Allowance{}, ctx.Err()
	})
	if stop, _, took := slow(ctx); stop || took >= allowanceWait {
		t.Fatalf("an interrupt: stop %v after %v", stop, took)
	}
}
