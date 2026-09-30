package agent

import (
	"encoding/json"
)

// CarriedSpend is what a session's record says it has spent: the parent's
// tokens at its last end plus every subagent's, and how many subagents it
// started. A session continued elsewhere starts its budget from it.
func CarriedSpend(events []Event) (tokens int64, spawned int) {
	events = Live(events)
	if end, ok := LastEnd(events); ok {
		tokens = int64(end.TokensIn + end.TokensOut)
	}
	for _, e := range events {
		switch e.Type {
		case EvSubagentSpawned:
			spawned++
		case EvSubagentReturn:
			var r struct {
				TokensIn  int `json:"tokens_in"`
				TokensOut int `json:"tokens_out"`
			}
			if json.Unmarshal(e.Payload, &r) == nil {
				tokens += int64(r.TokensIn + r.TokensOut)
			}
		}
	}
	return tokens, spawned
}

// Orphaned reports whether a record was left open by a process that went
// away: its last run never recorded an end, or it ended with background
// children that never recorded their return.
func Orphaned(events []Event) bool {
	if len(events) == 0 {
		return false
	}
	end, ok := LastEnd(events)
	if !ok || events[len(events)-1].Type != EvSessionEnded && lastEndSeq(events) < lastRunSeq(events) {
		return true
	}
	return end.Background > 0 || len(unreturned(events)) > 0
}

func lastEndSeq(events []Event) int64 {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == EvSessionEnded {
			return events[i].Seq
		}
	}
	return 0
}

// lastRunSeq is the last event a run writes: its messages, calls and model
// calls. Background work after an end does not count.
func lastRunSeq(events []Event) int64 {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case EvUserMessage, EvAgentMessage, EvModelCall, EvActionRequested, EvObservation, EvSessionWoken:
			return events[i].Seq
		}
	}
	return 0
}

// unreturned are the background children spawned with no return recorded.
func unreturned(events []Event) []string {
	var order []string
	open := map[string]bool{}
	for _, e := range events {
		var p struct {
			Background bool   `json:"background"`
			TaskID     string `json:"task_id"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || !p.Background || p.TaskID == "" {
			continue
		}
		switch e.Type {
		case EvSubagentSpawned:
			if !open[p.TaskID] {
				order = append(order, p.TaskID)
			}
			open[p.TaskID] = true
		case EvSubagentReturn:
			delete(open, p.TaskID)
		}
	}
	var out []string
	for _, id := range order {
		if open[id] {
			out = append(out, id)
		}
	}
	return out
}

// Reconcile closes what a crashed process left open in a session's record:
// each background child it lost is recorded as returned lost in the parent
// and, in its own record, as ended shutdown and recovered; then the parent
// gets a recovered, closing end. store holds both records; seq is the
// parent's last. Nothing is reconciled twice: a second call finds nothing open.
func Reconcile(store Store, sessionID string, events []Event) error {
	if !Orphaned(events) {
		return nil
	}
	parent := NewRecorder(store, sessionID, "")
	parent.Advance(events[len(events)-1].Seq)
	for _, id := range unreturned(events) {
		child := NewRecorder(store, id, sessionID)
		if evs, err := store.Events(id); err == nil && len(evs) > 0 {
			child.Advance(evs[len(evs)-1].Seq)
			if last := evs[len(evs)-1]; last.Type != EvSessionEnded {
				if _, err := child.Record(EvSessionEnded, ActorSystem, Trusted, SessionEnded{Reason: TermShutdown, Recovered: true}); err != nil {
					return err
				}
			}
		}
		if _, err := parent.Record(EvSubagentReturn, ActorSystem, Trusted, map[string]any{
			"session": id, "task_id": id, "background": true, "reason": string(TermLost),
		}); err != nil {
			return err
		}
	}
	end, ok := LastEnd(events)
	settled := end.Background > 0 || len(unreturned(events)) > 0
	if !ok || lastEndSeq(events) < lastRunSeq(events) {
		end = SessionEnded{Reason: TermShutdown, Turns: end.Turns, TokensIn: end.TokensIn, TokensOut: end.TokensOut}
	}
	end.Background, end.Settled, end.Recovered = 0, settled, true
	_, err := parent.Record(EvSessionEnded, ActorSystem, Trusted, end)
	return err
}
