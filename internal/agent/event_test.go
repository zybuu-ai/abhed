package agent

import (
	"fmt"
	"testing"
	"time"
)

// The conversation's last activity is a message, a reply or the agent's own
// call; a person's call at the workbench, a terminal, a rename and a model
// switch are not.
func TestLastAtFollowsTheConversationOnly(t *testing.T) {
	m := NewMemStore()
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	add := func(n int, typ EventType, actor Actor) {
		_ = m.Append(Event{ID: fmt.Sprint(n), SessionID: "s", Seq: int64(n), Type: typ, Actor: actor, CreatedAt: at.Add(time.Duration(n) * time.Minute)})
	}
	if _, ok := m.LastAt("s"); ok {
		t.Fatal("an empty session has a last activity")
	}
	add(1, EvSessionStarted, ActorSystem)
	if _, ok := m.LastAt("s"); ok {
		t.Fatal("a session with no conversation has a last activity")
	}
	add(2, EvUserMessage, ActorUser)
	add(3, EvActionRequested, ActorAgent)
	add(4, EvObservation, ActorTool)
	add(5, EvAgentMessage, ActorAgent)
	add(6, EvSessionEnded, ActorSystem)
	add(7, EvActionRequested, ActorUser) // the person's terminal at the workbench
	add(8, EvObservation, ActorTool)
	add(9, EvTerminalInput, ActorUser)
	add(10, EvSessionRenamed, ActorUser)
	add(11, EvSessionNamed, ActorUser)
	add(12, EvModelSwitched, ActorUser)
	if got, _ := m.LastAt("s"); !got.Equal(at.Add(5 * time.Minute)) {
		t.Fatalf("last activity %v, want the agent's reply at %v", got, at.Add(5*time.Minute))
	}
}
