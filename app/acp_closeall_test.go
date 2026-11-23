package app

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type idleACPAgent struct{}

func (idleACPAgent) Run(context.Context, string) (string, error) { return "", nil }
func (idleACPAgent) Steer(string)                                {}
func (idleACPAgent) Flush(context.Context) error                 { return nil }
func (idleACPAgent) CancelTasks() int                            { return 0 }
func (idleACPAgent) Close()                                      {}

// Closing the connection while a prompt starts or ends reads the session's
// cancel under the lock the prompt writes it with; -race reports otherwise.
// A session is shut once, so each round closes a new one.
func TestACPCloseAllWhilePromptsStartAndEnd(t *testing.T) {
	var cur atomic.Pointer[acpSession]
	cur.Store(&acpSession{id: "s-1", agent: idleACPAgent{}})
	c := &acpConn{sessions: map[string]*acpSession{}}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s := cur.Load()
			_, cancel := context.WithCancel(context.Background())
			s.mu.Lock()
			s.cancel = cancel
			s.mu.Unlock()
			s.mu.Lock()
			s.cancel = nil
			s.mu.Unlock()
			cancel()
		}
	}()
	for i := 0; i < 200; i++ {
		s := &acpSession{id: "s-1", agent: idleACPAgent{}}
		cur.Store(s)
		c.sessMu.Lock()
		c.sessions["s-1"] = s
		c.sessMu.Unlock()
		c.closeAll()
	}
	close(stop)
	wg.Wait()
}

type closeCountAgent struct {
	idleACPAgent
	n *atomic.Int32
}

func (a closeCountAgent) Close() { a.n.Add(1) }

// A session dropped (a failed load) after every session was taken to be
// closed is shut once, not by both; and one opened after the close began is refused.
func TestACPSessionShutOnce(t *testing.T) {
	var n atomic.Int32
	s := &acpSession{id: "s-1", agent: closeCountAgent{n: &n}}
	c := &acpConn{sessions: map[string]*acpSession{"s-1": s}}
	c.closeAll()
	c.dropSession(s)
	if got := n.Load(); got != 1 {
		t.Fatalf("the session was shut %d times", got)
	}
	if c.addSession(&acpSession{id: "s-2", agent: idleACPAgent{}}) {
		t.Fatal("a session was added after every session was closed")
	}
}

// A session opened while every session is being closed is refused and shut
// by that close, before it ends, so the exit waits for its end too.
func TestACPSessionRefusedDuringCloseIsShutByIt(t *testing.T) {
	c := &acpConn{sessions: map[string]*acpSession{}}
	entered, gate := make(chan struct{}), make(chan struct{})
	c.beforeShut = func() { close(entered); <-gate }
	done := make(chan struct{})
	go func() { c.closeOnce(0); close(done) }()
	<-entered
	var n atomic.Int32
	if c.addSession(&acpSession{id: "s-late", agent: closeCountAgent{n: &n}}) {
		t.Fatal("a session was added while every session was being closed")
	}
	close(gate)
	<-done
	if got := n.Load(); got != 1 {
		t.Fatalf("the close ended with the refused session shut %d times", got)
	}
}

// The close waits once for the editor calls of all its sessions, against
// one deadline: calls that never end cost it summaryWait, not summaryWait
// for each session.
func TestACPCloseWaitsOnceForEditorCalls(t *testing.T) {
	c := &acpConn{sessions: map[string]*acpSession{}, comps: map[string]*acpCompleter{}}
	for _, id := range []string{"s-1", "s-2", "s-3"} {
		c.sessions[id] = &acpSession{id: id, agent: idleACPAgent{}}
		c.comps[id+"\x00next_edit"] = &acpCompleter{running: 1} // never counted
	}
	start := time.Now()
	c.closeAll()
	if took := time.Since(start); took > summaryWait+summaryWait/2 {
		t.Fatalf("the close took %v for three sessions' stuck calls; want about %v", took, summaryWait)
	}
}

// boundRig runs a connection with stdin held open and a close that never
// ends on its own, for the bounds on the exit.
func boundRig(t *testing.T) (c *acpConn, stop func(), inW *io.PipeWriter, entered, released, ran chan struct{}) {
	ctx, stop := context.WithCancel(context.Background())
	released, ran, entered = make(chan struct{}), make(chan struct{}), make(chan struct{})
	gate := make(chan struct{})
	inR, inW := io.Pipe()
	c = &acpConn{out: io.Discard, sessions: map[string]*acpSession{}, pending: map[int64]chan rpcMessage{}, ctx: ctx,
		busy: func() func() { var once sync.Once; return func() { once.Do(func() { close(released) }) } }}
	c.beforeShut = func() { close(entered); <-gate }
	go func() { _ = c.run(inR); close(ran) }()
	t.Cleanup(func() {
		stop()
		close(gate)
		_ = inW.Close()
		<-ran
	})
	return c, stop, inW, entered, released, ran
}

// closesWithin fails unless ch closes between lo and hi after from.
func closesWithin(t *testing.T, what string, ch chan struct{}, from time.Time, lo, hi time.Duration) {
	t.Helper()
	select {
	case <-ch:
		if took := time.Since(from); took < lo {
			t.Fatalf("%s after %v, before the close's bound", what, took)
		}
	case <-time.After(hi - time.Since(from)):
		t.Fatalf("%s not within %v", what, hi)
	}
}

// A stop signal while stdin stays open closes the sessions itself, and lets
// the exit go once that close ends or at signalCloseWait from its start,
// whichever is first.
func TestACPSignalCloseIsBounded(t *testing.T) {
	_, stop, _, entered, released, _ := boundRig(t)
	stopped := time.Now()
	stop()
	<-entered
	closesWithin(t, "the exit was let go", released, stopped, stdinGrace+signalCloseWait-200*time.Millisecond, stdinGrace+signalCloseWait+1500*time.Millisecond)
}

// A stop signal while a close stdin's end began is still running bounds it:
// the exit is let go signalCloseWait after the signal, not when the close
// ends (it never does here).
func TestACPSignalDuringInputCloseIsBounded(t *testing.T) {
	_, stop, inW, entered, released, ran := boundRig(t)
	_ = inW.Close()
	<-entered
	stopped := time.Now()
	stop()
	closesWithin(t, "run returned", ran, stopped, signalCloseWait-200*time.Millisecond, signalCloseWait+1500*time.Millisecond)
	closesWithin(t, "the exit was let go", released, stopped, 0, signalCloseWait+1500*time.Millisecond)
}
