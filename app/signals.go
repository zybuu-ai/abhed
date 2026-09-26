package app

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// stopWait bounds how long a stopped command waits for its work to end
// before it exits anyway; a container's removal can take most of it.
const stopWait = 30 * time.Second

// flushWait bounds how long rpc and acp wait for a run's events to be written
// before its reply, so an OnEvent that never returns cannot hold them.
const flushWait = 5 * time.Second

// stopSignals end a run from outside. An agent's command runs in a session of
// its own, so they reach it only through the cancel they cause.
func stopSignals() []os.Signal {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	// Under nohup a hang-up is ignored on purpose; catching it would undo that.
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}

// stoppedBy is the cancel cause a stop signal leaves on the context.
type stoppedBy struct{ sig os.Signal }

func (s stoppedBy) Error() string { return "stopped by " + s.sig.String() }

// stopCode is the exit status for a run a signal stopped, 128 plus its
// number, as a shell reports a process the signal ended.
func stopCode(ctx context.Context) (int, bool) {
	var s stoppedBy
	if !errors.As(context.Cause(ctx), &s) {
		return 0, false
	}
	if n, ok := s.sig.(syscall.Signal); ok {
		return 128 + int(n), true
	}
	return 1, true
}

// onStop is what a command does after the first stop signal cancels its context.
type onStop int

const (
	// stopReturns: the command returns on the cancel; a second signal takes
	// its default action, for a person who does not want to wait.
	stopReturns onStop = iota
	// stopDrains: the command drains on the cancel, and later signals are
	// caught and ignored, so a second SIGTERM cannot cut a drain short.
	stopDrains
	// stopExits: the command's input loop does not end on a cancel, so the
	// process exits once the work it was told about has ended, or at a
	// second signal.
	stopExits
)

// stopper cancels a context on a stop signal and then does what its onStop says.
type stopper struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	sigs   chan os.Signal
	done   chan struct{}

	mu     sync.Mutex
	active int
	idle   chan struct{} // closed while nothing is active
}

func cancelOnStop(then onStop) *stopper {
	ctx, cancel := context.WithCancelCause(context.Background())
	s := &stopper{ctx: ctx, cancel: cancel, sigs: make(chan os.Signal, 2), done: make(chan struct{}), idle: make(chan struct{})}
	close(s.idle)
	signal.Notify(s.sigs, stopSignals()...)
	go s.watch(then)
	return s
}

func (s *stopper) watch(then onStop) {
	var sig os.Signal
	select {
	case sig = <-s.sigs:
	case <-s.done:
		return
	}
	s.cancel(stoppedBy{sig})
	switch then {
	case stopReturns:
		signal.Stop(s.sigs)
		return
	case stopDrains:
		for {
			select {
			case <-s.sigs:
			case <-s.done:
				return
			}
		}
	}
	code, _ := stopCode(s.ctx)
	s.mu.Lock()
	idle := s.idle
	s.mu.Unlock()
	select {
	case <-idle:
	case <-s.sigs:
	case <-time.After(stopWait):
	}
	os.Exit(code)
}

// busy marks work under way until the returned func is called, so an exit on
// a signal waits for it to record how it ended.
func (s *stopper) busy() func() {
	s.mu.Lock()
	if s.active == 0 {
		s.idle = make(chan struct{})
	}
	s.active++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.active--
			if s.active == 0 {
				close(s.idle)
			}
			s.mu.Unlock()
		})
	}
}

func (s *stopper) stop() {
	signal.Stop(s.sigs)
	close(s.done)
	s.cancel(nil)
}
