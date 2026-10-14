package server

import (
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A run suspended for a TurnMover does not give its claim up as it ends:
// the row stays held until SuspendRunning parks it.
func TestSuspendedRunKeepsItsClaim(t *testing.T) {
	h := newHOServer(t, agent.NewMemStore(), newBlocker(), tempWorkspace(t))
	id := h.start("hello")
	waitUntil(t, "the turn to end", func() bool {
		_, ok := agent.LastEnd(h.events(id))
		return ok && h.live(id) != nil && h.state(id) != "running"
	})
	live := h.live(id)
	released := false
	live.mu.Lock()
	live.Reason, live.beatStop = agent.TermHandedOver, func() { released = true }
	live.mu.Unlock()
	h.s.releaseNodeIfQuiet(live)
	live.mu.Lock()
	kept := live.beatStop != nil
	live.mu.Unlock()
	if released || !kept {
		t.Fatal("a suspended run released its claim before the row was parked")
	}
	live.mu.Lock()
	live.Reason = agent.TermCompleted
	live.mu.Unlock()
	h.s.releaseNodeIfQuiet(live)
	if !released {
		t.Fatal("a finished run kept its claim")
	}
}
