package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
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
