package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/zybuu-ai/abhed/internal/model"
)

// ErrHandOver, given as a context cancel cause, stops a session's run so that
// another process can take the turn over: a request waiting for an answer is
// left open, and no session.ended is written. A subagent's run it reaches ends
// as a shutdown, since a child does not move.
var ErrHandOver = errors.New("handed over to another process")

// TermHandedOver is what a run stopped by ErrHandOver returns. It is never
// recorded: the record stays open for the process that takes the turn over.
const TermHandedOver TerminalReason = "handed_over"

// EvTurnContinued marks a turn another process left running being taken
// over here; see TurnContinued.
const EvTurnContinued EventType = "turn.continued"

// TurnContinued is the payload of EvTurnContinued: the step the turn goes on
// from, the calls asked again as new calls (each old id with its new one),
// and the calls whose outcome is unknown, which are not run again.
type TurnContinued struct {
	ThroughSeq int64             `json:"through_seq"`
	AskedAgain map[string]string `json:"asked_again,omitempty"`
	Unknown    []string          `json:"outcome_unknown,omitempty"`
}

// OpenCall is a call a stopped run left without a result.
type OpenCall struct {
	Request Event
	Call    model.ToolCall
	// Approved is set when an approval was recorded: the call may have run.
	Approved bool
}

// OpenTurn is a run that never recorded its end, and its calls left open.
type OpenTurn struct {
	ThroughSeq int64
	Calls      []OpenCall
	// Others are a person's own calls and a pipeline's steps left open: not
	// the model's to ask again, but each still owed an outcome.
	Others []OpenCall
}

// InFlight reports the run a record left going: one that wrote after its
// last end, with the calls of its own that have no result, in call order.
func InFlight(events []Event) (OpenTurn, bool) {
	events = Live(events)
	if len(events) == 0 || lastEndSeq(events) >= lastRunSeq(events) {
		return OpenTurn{}, false
	}
	open := OpenTurn{ThroughSeq: events[len(events)-1].Seq}
	from := lastEndSeq(events)
	var all []OpenCall
	others := map[string]bool{}
	byID := map[string]int{}
	settled := map[string]bool{}
	for _, e := range events {
		if e.Seq <= from {
			continue
		}
		switch e.Type {
		case EvActionRequested:
			var a ActionRequested
			if json.Unmarshal(e.Payload, &a) != nil {
				continue
			}
			// A person's own call and a pipeline's step are not the model's.
			others[a.CallID] = e.Actor == ActorUser || a.Via != ""
			byID[a.CallID] = len(all)
			all = append(all, OpenCall{Request: e,
				Call: model.ToolCall{ID: a.CallID, Name: a.Tool, Args: objectOr(a.Args)}})
		case EvActionApproved, EvActionDenied, EvObservation:
			var p struct {
				CallID string `json:"call_id"`
			}
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			i, ok := byID[p.CallID]
			switch {
			case !ok:
			case e.Type == EvActionApproved:
				all[i].Approved = true
			default:
				settled[p.CallID] = true
			}
		}
	}
	for _, c := range all {
		switch {
		case settled[c.Call.ID]:
		case others[c.Call.ID]:
			open.Others = append(open.Others, c)
		default:
			open.Calls = append(open.Calls, c)
		}
	}
	return open, true
}

// notRunStopped is why an open call that never ran is refused.
const notRunStopped = "Not run: the process asking for it stopped."

// settleOpenCalls gives each call its one outcome without running it: one
// that may have run is answered as outcome unknown, any other is refused.
func settleOpenCalls(rec *Recorder, calls []OpenCall) error {
	for _, c := range calls {
		var err error
		if c.Approved {
			_, err = rec.Record(EvObservation, ActorSystem, Trusted, Observation{CallID: c.Call.ID, Tool: c.Call.Name,
				Content: unknownOutcome, IsError: true, OutcomeUnknown: true})
		} else {
			_, err = rec.Record(EvActionDenied, ActorSystem, Trusted, map[string]string{
				"call_id": c.Call.ID, "reason": notRunStopped, "step": "handover", "by": BySystem,
			})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// unknownOutcome answers a call that may have run before its process stopped.
const unknownOutcome = "Outcome unknown: the process running this call stopped before its result was recorded, " +
	"so it may have run, in part or in full. It was not run again. Check its effect before repeating it; " +
	"the same call again is asked of a person first."

// askedAgainNote answers an open call that never ran, now asked again as a new call.
func askedAgainNote(id string) string {
	return "Not run here: the process asking for it stopped. It was asked again as call " + id + "."
}

// mayHaveRun holds the calls whose outcome is unknown, by tool and arguments.
type mayHaveRun struct {
	mu   sync.Mutex
	keys map[string]bool
}

func runKey(tool string, args json.RawMessage) string { return tool + "\x00" + string(args) }

func (m *mayHaveRun) add(tool string, args json.RawMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keys == nil {
		m.keys = map[string]bool{}
	}
	m.keys[runKey(tool, args)] = true
}

func (m *mayHaveRun) has(tool string, args json.RawMessage) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keys[runKey(tool, args)]
}

// NoteUnknownOutcomes arms the ask for every call the record answered as
// outcome unknown, so a session continued anywhere asks before running one again.
func (l *Loop) NoteUnknownOutcomes(events []Event) {
	requests := map[string]ActionRequested{}
	for _, e := range Live(events) {
		switch e.Type {
		case EvActionRequested:
			var a ActionRequested
			if json.Unmarshal(e.Payload, &a) == nil {
				requests[a.CallID] = a
			}
		case EvObservation:
			var o Observation
			if json.Unmarshal(e.Payload, &o) == nil && o.OutcomeUnknown {
				if a, ok := requests[o.CallID]; ok {
					l.unknown.add(a.Tool, a.Args)
				}
			}
		}
	}
}

// handingOver reports whether ctx was stopped to hand the session's turn over;
// a subagent's run is not handed over but ends.
func (l *Loop) handingOver(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrHandOver) && (l.Recorder == nil || l.Recorder.parentID == "")
}

// handedOver stops a run being handed over: what was queued and not yet
// delivered is recorded as dropped, and nothing ends.
func (l *Loop) handedOver() TerminalReason {
	for _, q := range l.takeSteering() {
		l.record(EvMessageDropped, ActorSystem, DroppedMessage{
			QueueID: q.ID, ClientID: q.ClientID, Text: q.Text, QueuedAt: q.At,
			Reason: "the session moved to another process before it was delivered; send it again",
		})
	}
	return TermHandedOver
}

// ContinueTurn takes over a turn another process left running, from its
// record: the history must already be set from the same events (SetHistory
// with Fork). Each call that never ran is asked again as a new call, through
// policy and approval as any call is; a call that may have run is answered as
// outcome unknown and not run again, and the same call later asks a person
// first. Then the run goes on, the model seeing those results.
func (l *Loop) ContinueTurn(ctx context.Context, events []Event, open OpenTurn) (_ TerminalReason, err error) {
	defer func() { err = l.redactErr(err) }()
	l.StopSuggestion()
	l.runMu.Lock()
	defer l.unlockRun()
	if l.OwnerActive != nil {
		// No one asked for this run here: it acts for the owner only while they may.
		ctx = context.WithValue(ctx, ownerGateKey{}, l.OwnerActive)
	}
	l.NoteUnknownOutcomes(events)
	note := TurnContinued{ThroughSeq: open.ThroughSeq}
	// Calls are asked again only into the turn that ends the history, so
	// their results follow its own in call order.
	turn := l.openTurnAt(open)
	var again []model.ToolCall
	for i, c := range open.Calls {
		if c.Approved {
			note.Unknown = append(note.Unknown, c.Call.ID)
			continue
		}
		if turn < 0 {
			continue
		}
		nc := c.Call
		nc.ID = fmt.Sprintf("%s_again%d", c.Call.ID, i+1)
		again = append(again, nc)
		if note.AskedAgain == nil {
			note.AskedAgain = map[string]string{}
		}
		note.AskedAgain[c.Call.ID] = nc.ID
	}
	if _, err := l.Recorder.Record(EvTurnContinued, ActorSystem, Trusted, note); err != nil {
		return TermError, err
	}
	// Each old request gets its one outcome. A call that never ran is refused
	// here, before its new call is asked; one that may have run is answered
	// after the new calls, so a rebuild keeps them all in the one turn.
	results := map[string]string{}
	for _, c := range open.Calls {
		why := notRunStopped
		switch {
		case c.Approved:
			l.unknown.add(c.Call.Name, c.Call.Args)
			results[c.Call.ID] = unknownOutcome
			continue
		case note.AskedAgain[c.Call.ID] != "":
			why = askedAgainNote(note.AskedAgain[c.Call.ID])
		}
		l.record(EvActionDenied, ActorSystem, map[string]string{
			"call_id": c.Call.ID, "reason": why, "step": "handover", "by": BySystem,
		})
		results[c.Call.ID] = "Not run: denied — " + why // as a rebuild answers a refused call
	}
	if err := l.recordFailure(); err != nil {
		return TermError, err
	}
	for i := range l.messages {
		if m := &l.messages[i]; m.Role == model.RoleTool {
			if r, ok := results[m.ToolCallID]; ok {
				m.Content, m.IsError = r, true
			}
		}
	}
	l.startMessage()
	var terminal TerminalReason
	if len(again) > 0 {
		l.messages[turn].ToolCalls = append(append([]model.ToolCall{}, l.messages[turn].ToolCalls...), again...)
		terminal = l.runCalls(ctx, again)
	}
	for _, c := range open.Calls {
		if c.Approved {
			l.record(EvObservation, ActorSystem, Observation{CallID: c.Call.ID, Tool: c.Call.Name,
				Content: unknownOutcome, IsError: true, OutcomeUnknown: true})
		}
	}
	if terminal != "" {
		return l.finishStopped(ctx, terminal), nil
	}
	// The model had answered and only the end was not written: the turn is
	// over, and asking the model again would answer twice.
	if n := len(l.messages); len(open.Calls) == 0 && n > 0 &&
		l.messages[n-1].Role == model.RoleAssistant && len(l.messages[n-1].ToolCalls) == 0 && !l.hasWork() {
		return l.finish(TermCompleted), nil
	}
	return l.run(ctx)
}

// openTurnAt is the index of the assistant turn holding the open calls when
// it and its results end the history, else -1.
func (l *Loop) openTurnAt(open OpenTurn) int {
	ids := map[string]bool{}
	for _, c := range open.Calls {
		ids[c.Call.ID] = true
	}
	for i := len(l.messages) - 1; i >= 0; i-- {
		m := l.messages[i]
		if m.Role == model.RoleTool {
			continue
		}
		for _, c := range m.ToolCalls {
			if ids[c.ID] {
				return i
			}
		}
		return -1
	}
	return -1
}
