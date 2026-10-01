package app

import (
	"context"
	"sync"
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
func TestACPCloseAllWhilePromptsStartAndEnd(t *testing.T) {
	s := &acpSession{id: "s-1", agent: idleACPAgent{}}
	c := &acpConn{sessions: map[string]*acpSession{"s-1": s}}
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
		c.sessMu.Lock()
		c.sessions["s-1"] = s
		c.sessMu.Unlock()
		c.closeAll()
	}
	close(stop)
	wg.Wait()
}
