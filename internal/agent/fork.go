package agent

import (
	"encoding/json"
	"fmt"

	"github.com/zybuu-ai/abhed/internal/model"
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
	var msgs []model.Message
	// Tool calls are recorded as their own events and have to be reattached to
	// the assistant turn that made them, or the model sees results answering
	// questions nothing asked.
	pendingCalls := map[string]model.ToolCall{}
	var lastAssistant *model.Message

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
			call := model.ToolCall{ID: a.CallID, Name: a.Tool, Args: a.Args}
			pendingCalls[a.CallID] = call
			// Attach to the assistant turn that produced it, creating one when
			// the model called a tool without saying anything first.
			if lastAssistant == nil {
				msgs = append(msgs, model.Message{Role: model.RoleAssistant})
				lastAssistant = &msgs[len(msgs)-1]
			}
			lastAssistant.ToolCalls = append(lastAssistant.ToolCalls, call)

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

	// A call with no result would leave the model waiting on an answer that
	// never comes. Truncate to the last complete exchange instead.
	if len(pendingCalls) > 0 {
		msgs = truncateToComplete(msgs)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("nothing to fork from: no messages at or before seq %d", throughSeq)
	}
	return msgs, nil
}

// truncateToComplete drops a trailing assistant turn whose tool results are
// missing, so a forked conversation never opens with an unanswered call.
func truncateToComplete(msgs []model.Message) []model.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleAssistant && len(msgs[i].ToolCalls) > 0 {
			answered := map[string]bool{}
			for _, m := range msgs[i+1:] {
				if m.Role == model.RoleTool {
					answered[m.ToolCallID] = true
				}
			}
			complete := true
			for _, c := range msgs[i].ToolCalls {
				if !answered[c.ID] {
					complete = false
					break
				}
			}
			if !complete {
				return msgs[:i]
			}
			return msgs
		}
	}
	return msgs
}

// Restore seeds a loop with a reconstructed conversation, for resuming or
// forking a session.
func (l *Loop) Restore(msgs []model.Message) {
	l.messages = msgs
}
