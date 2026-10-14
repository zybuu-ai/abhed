package app

import (
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

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
