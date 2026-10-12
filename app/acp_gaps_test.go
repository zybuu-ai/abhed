package app

import (
	"context"
	"testing"
)

// A result the last turn never recorded will not come: a new turn forgets the
// approved id-less calls still awaiting one, so a later result of the same
// tool is not named by a call from an earlier turn.
func TestNewTurnForgetsIdlessCallsAwaitingAResult(t *testing.T) {
	s := &acpSession{ranIdless: []idlessCall{{id: "r-old", tool: "bash"}}}
	s.mu.Lock()
	s.beginTurnLocked(context.Background(), func() {})
	s.mu.Unlock()
	if got := s.takeIdless("bash"); got != "" {
		t.Fatalf("a new turn's result was named by the last turn's call %q", got)
	}
}
