package abhed

import (
	"context"
	"sync"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// wakeRuns tracks the wake runs in progress, so a stop or Close ends them.
type wakeRuns struct {
	mu     sync.Mutex
	closed bool
	next   int
	cancel map[int]context.CancelFunc
}

// hookWake lets finished background work start a wake run, with host
// starting it when given; only the session's own results trigger one.
func (a *Agent) hookWake(host func([]string, func(context.Context) (string, error)) bool) {
	a.loop.Background.SetHooks(agent.BackgroundHooks{
		CanWake: a.canWake,
		Wake: func(ids []string) bool {
			run := func(ctx context.Context) (string, error) { return a.wakeRun(ctx, ids) }
			if host != nil {
				return host(ids, run)
			}
			go func() { _, _ = run(context.Background()) }()
			return true
		},
	})
}

// canWake refuses a wake once Close has begun.
func (a *Agent) canWake() (bool, string) {
	a.wakes.mu.Lock()
	defer a.wakes.mu.Unlock()
	if a.wakes.closed {
		return false, "closed"
	}
	return true, ""
}

// wakeRun is one wake run for the results of ids, ended early by a stop or Close.
func (a *Agent) wakeRun(ctx context.Context, ids []string) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.wakes.mu.Lock()
	if a.wakes.closed {
		a.wakes.mu.Unlock()
		return "", agent.ErrNothingToWake
	}
	if a.wakes.cancel == nil {
		a.wakes.cancel = map[int]context.CancelFunc{}
	}
	a.wakes.next++
	n := a.wakes.next
	a.wakes.cancel[n] = cancel
	a.wakes.mu.Unlock()
	defer func() {
		a.wakes.mu.Lock()
		delete(a.wakes.cancel, n)
		a.wakes.mu.Unlock()
	}()
	defer a.startRun()()
	reason, err := a.loop.RunWoken(ctx, agent.Wake{By: "policy", TaskIDs: ids})
	if err != nil {
		return "", err
	}
	if reason != agent.TermCompleted && reason != agent.TermWakeLimit {
		return a.lastMessage(), &EndedError{Reason: reason}
	}
	return a.lastMessage(), nil
}

// stopWake ends the wake runs in progress.
func (a *Agent) stopWake() {
	a.wakes.mu.Lock()
	defer a.wakes.mu.Unlock()
	for _, c := range a.wakes.cancel {
		c()
	}
}

// endWakes ends the wake runs in progress and refuses any more.
func (a *Agent) endWakes() {
	a.wakes.mu.Lock()
	a.wakes.closed = true
	a.wakes.mu.Unlock()
	a.stopWake()
}
