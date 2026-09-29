package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Fork reconstructs a conversation from a session's events, up to and including
// a sequence number.
//
// This is what makes a wrong turn cheap. Without it, an agent that went the
// wrong way three turns ago leaves two options: keep going and hope, or start
// over and re-establish everything it had already worked out. Forking keeps the
// part that was right and abandons only the part that was not.
//
// It works because the event log is the conversation, not a description of it.
// Nothing extra has to be stored for this: the messages are derivable from
// events that were already being recorded for audit, which is the second time
// event sourcing has paid for itself here.
func Fork(events []Event, throughSeq int64) ([]model.Message, error) {
	events = Live(events)
	var msgs []model.Message
	// Tool calls are recorded as their own events and have to be reattached to
	// the assistant turn that made them, or the model sees results answering
	// questions nothing asked.
	pendingCalls := map[string]model.ToolCall{}
	denied := map[string]string{}
	var lastAssistant *model.Message
	// Calls answered past the cut: a fork inside their turn drops that turn.
	answeredLater := map[string]bool{}
	for _, ev := range events {
		if throughSeq > 0 && ev.Seq > throughSeq && (ev.Type == EvObservation || ev.Type == EvActionDenied) {
			var a struct {
				CallID string `json:"call_id"`
			}
			if json.Unmarshal(ev.Payload, &a) == nil {
				answeredLater[a.CallID] = true
			}
		}
	}

	for _, ev := range events {
		if throughSeq > 0 && ev.Seq > throughSeq {
			break
		}
		switch ev.Type {
		case EvUserMessage:
			var m Message
			if json.Unmarshal(ev.Payload, &m) != nil {
				continue
			}
			msgs = append(msgs, model.Message{Role: model.RoleUser, Content: m.Text})
			lastAssistant = nil

		case EvAgentMessage:
			var m Message
			if json.Unmarshal(ev.Payload, &m) != nil {
				continue
			}
			msgs = append(msgs, model.Message{Role: model.RoleAssistant, Content: m.Text})
			lastAssistant = &msgs[len(msgs)-1]

		case EvActionRequested:
			// A person's own call at the workbench never entered the model's
			// conversation, so a rebuilt one leaves it out too.
			if ev.Actor == ActorUser {
				continue
			}
			var a ActionRequested
			if json.Unmarshal(ev.Payload, &a) != nil {
				continue
			}
			call := model.ToolCall{ID: a.CallID, Name: a.Tool, Args: objectOr(a.Args)}
			pendingCalls[a.CallID] = call
			// Attach to the assistant turn that produced it, creating one when
			// the model called a tool without saying anything first.
			if lastAssistant == nil {
				msgs = append(msgs, model.Message{Role: model.RoleAssistant})
				lastAssistant = &msgs[len(msgs)-1]
			}
			lastAssistant.ToolCalls = append(lastAssistant.ToolCalls, call)

		case EvActionDenied:
			var d struct {
				CallID string `json:"call_id"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal(ev.Payload, &d) == nil {
				// Settled by the refusal; answeredLater decides a cut, this keeps pending exact.
				denied[d.CallID] = d.Reason
				delete(pendingCalls, d.CallID)
			}

		case EvModelCall:
			// Each model call is a new assistant turn, even one whose calls were all refused.
			lastAssistant = nil

		case EvSubagentNotice:
			// A background result: the task_status call and its result, as the
			// live conversation received them.
			var n Notice
			if json.Unmarshal(ev.Payload, &n) != nil || n.CallID == "" {
				continue
			}
			msgs = append(msgs, noticeMessages(n)...)
			lastAssistant = nil

		case EvObservation:
			var o Observation
			if json.Unmarshal(ev.Payload, &o) != nil {
				continue
			}
			if _, ok := pendingCalls[o.CallID]; !ok {
				continue // a result for a call that was not replayed
			}
			delete(pendingCalls, o.CallID)
			msgs = append(msgs, model.Message{
				Role: model.RoleTool, ToolCallID: o.CallID,
				Content: o.Content, IsError: o.IsError,
			})
			lastAssistant = nil
		}
	}

	// A fork inside a turn drops the turn whose results lie past the cut; any
	// other call with no result, refused, never run or lost, is answered as such.
	cutCalls := map[string]model.ToolCall{}
	for id, c := range pendingCalls {
		if answeredLater[id] {
			cutCalls[id] = c
		}
	}
	msgs = truncateAt(msgs, cutCalls)
	msgs = answerAll(msgs, denied)
	if len(msgs) == 0 {
		return nil, fmt.Errorf("nothing to fork from: no messages at or before seq %d", throughSeq)
	}
	return msgs, nil
}

// answerAll gives every call without a result one saying so, and puts each
// turn's results in call order, which providers matching by position need.
func answerAll(msgs []model.Message, denied map[string]string) []model.Message {
	out := make([]model.Message, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		// A rebuilt request carries one result per call, and only for that turn's calls.
		if msgs[i].Role == model.RoleTool {
			continue
		}
		out = append(out, msgs[i])
		if msgs[i].Role != model.RoleAssistant || len(msgs[i].ToolCalls) == 0 {
			continue
		}
		results := map[string]model.Message{}
		for i+1 < len(msgs) && msgs[i+1].Role == model.RoleTool {
			i++
			if _, dup := results[msgs[i].ToolCallID]; !dup {
				results[msgs[i].ToolCallID] = msgs[i]
			}
		}
		for _, c := range out[len(out)-1].ToolCalls {
			if r, ok := results[c.ID]; ok {
				out = append(out, r)
				delete(results, c.ID)
				continue
			}
			text := "No result was recorded; it may have run."
			if why, ok := denied[c.ID]; ok {
				text = "Not run: denied — " + why
			}
			out = append(out, model.Message{Role: model.RoleTool, ToolCallID: c.ID, Content: text, IsError: true})
		}
	}
	return out
}

// truncateAt drops the conversation from the turn holding the first call still
// waiting on a result, so a fork never opens with a call answered past its cut.
func truncateAt(msgs []model.Message, pending map[string]model.ToolCall) []model.Message {
	for i, m := range msgs {
		for _, c := range m.ToolCalls {
			if _, ok := pending[c.ID]; ok {
				return msgs[:i]
			}
		}
	}
	return msgs
}

// Forked is the payload of EvForked: the conversation went on from ThroughSeq.
type Forked struct {
	ThroughSeq int64 `json:"through_seq"`
}

// Live is the record as the conversation now stands: each fork marker drops
// the branch it abandoned, the events after its step and before the marker.
func Live(events []Event) []Event {
	out := make([]Event, 0, len(events))
	for _, ev := range events {
		if ev.Type == EvForked {
			var f Forked
			if json.Unmarshal(ev.Payload, &f) == nil {
				n := len(out)
				for n > 0 && out[n-1].Seq > f.ThroughSeq {
					n--
				}
				out = out[:n]
			}
			continue
		}
		out = append(out, ev)
	}
	return out
}

// ForkTo rebuilds the conversation up to step seq, records that what came after
// was abandoned, and returns how many messages were kept.
func (l *Loop) ForkTo(events []Event, seq int64) (int, error) {
	live := Live(events)
	if seq <= 0 && len(live) > 0 {
		seq = live[len(live)-1].Seq // 0 keeps the whole conversation
	}
	if !slices.ContainsFunc(live, func(e Event) bool { return e.Seq == seq }) {
		return 0, fmt.Errorf("step %d is not in the conversation as it stands; an earlier fork abandoned it, or there is no such step", seq)
	}
	msgs, err := Fork(live, seq)
	if err != nil {
		return 0, err
	}
	l.runMu.Lock()
	defer l.runMu.Unlock()
	if _, err := l.Recorder.Record(EvForked, ActorUser, Trusted, Forked{ThroughSeq: seq}); err != nil {
		return 0, err
	}
	l.messages = msgs
	return len(msgs), nil
}

// Restore seeds a loop with a reconstructed conversation, for resuming or
// forking a session.
func (l *Loop) Restore(msgs []model.Message) {
	l.messages = msgs
}

// objectOr returns args when they are one JSON object, and {} otherwise:
// providers refuse a replayed call whose arguments are anything else.
func objectOr(args json.RawMessage) json.RawMessage {
	if _, err := tools.DecodeArgs(args); err != nil || len(bytes.TrimSpace(args)) == 0 || bytes.Equal(bytes.TrimSpace(args), []byte("null")) {
		return json.RawMessage(`{}`)
	}
	return args
}
