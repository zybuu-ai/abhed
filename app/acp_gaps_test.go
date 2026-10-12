package app

import (
	"context"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
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

// waitDrawn holds a command until what the conversation recorded is drawn,
// so the footer it refreshes next reads the model the command switched to.
func TestWaitDrawnWaitsForTheRecordToBeDrawn(t *testing.T) {
	store := agent.NewMemStore()
	rec := agent.NewRecorder(store, "s", "")
	for range 3 {
		if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, map[string]string{"text": "x"}); err != nil {
			t.Fatal(err)
		}
	}
	c := &cliState{loop: &agent.Loop{Recorder: rec}, unfollow: func() {}}
	last := rec.LastAppended()
	go func() {
		// Drawing catches up one event at a time, as a follower does.
		for seq := int64(1); seq <= last; seq++ {
			time.Sleep(5 * time.Millisecond)
			c.rendered.Store(seq)
		}
	}()
	c.waitDrawn()
	if got := c.rendered.Load(); got < last {
		t.Fatalf("waitDrawn returned with %d of %d events drawn", got, last)
	}
}
