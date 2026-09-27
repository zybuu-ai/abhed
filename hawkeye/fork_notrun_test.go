package hawkeye

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func evAt(seq int64, t agent.EventType, actor agent.Actor, payload any) agent.Event {
	raw, _ := json.Marshal(payload)
	return agent.Event{ID: string(rune('a' + seq)), SessionID: "s1", Seq: seq, Type: t, Actor: actor,
		Payload: raw, CreatedAt: time.Unix(1_700_000_000+seq, 0).UTC()}
}

// A call the turn ended before running is answered in the record but not run,
// and a fork marker is a step like any other: no gap, the audit kept whole.
func TestNotRunAnswerAndForkMarker(t *testing.T) {
	events := []agent.Event{
		evAt(1, agent.EvUserMessage, agent.ActorUser, agent.Message{Text: "go"}),
		evAt(2, agent.EvActionRequested, agent.ActorAgent, agent.ActionRequested{CallID: "c1", Tool: "glob"}),
		evAt(3, agent.EvActionApproved, agent.ActorSystem, map[string]string{"call_id": "c1", "step": "mode", "by": agent.ByPolicy}),
		evAt(4, agent.EvObservation, agent.ActorSystem, agent.Observation{CallID: "c1", Tool: "glob",
			Content: "Not run: the turn ended (user_interrupt) before this call ran.", IsError: true, NotRun: true}),
		evAt(5, agent.EvSessionEnded, agent.ActorSystem, agent.SessionEnded{Reason: agent.TermUserInterrupt}),
		evAt(6, agent.EvForked, agent.ActorUser, agent.Forked{ThroughSeq: 1}),
		evAt(7, agent.EvUserMessage, agent.ActorUser, agent.Message{Text: "again"}),
		evAt(8, agent.EvSessionEnded, agent.ActorSystem, agent.SessionEnded{Reason: agent.TermCompleted}),
	}
	r := Analyze("s1", events)
	if len(r.Calls) != 1 || r.Calls[0].Ran {
		t.Fatalf("the call that never ran: %+v", r.Calls)
	}
	if len(r.Integrity.Gaps) != 0 || r.Integrity.FirstSeq != 1 || r.Integrity.LastSeq != 8 {
		t.Fatalf("integrity of a record with a fork: %+v", r.Integrity)
	}
	for _, f := range r.Findings {
		if f.Severity == Critical {
			t.Errorf("a critical finding on a whole record: %+v", f)
		}
	}
}
