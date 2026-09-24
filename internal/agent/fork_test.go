package agent

import (
	"encoding/json"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
)

func ev(seq int64, t EventType, payload any) Event {
	b, _ := json.Marshal(payload)
	return Event{Seq: seq, Type: t, Payload: b}
}

// A person's call at the workbench, and its result, are not part of the
// model's conversation, so a continued session does not see them either.
func TestForkLeavesOutThePersonsOwnCalls(t *testing.T) {
	mine := ev(9, EvActionRequested, ActionRequested{CallID: "u1", Tool: "bash", Args: json.RawMessage(`{"command":"ls"}`)})
	mine.Actor = ActorUser
	events := append(fullSession(), mine, ev(10, EvObservation, Observation{CallID: "u1", Tool: "bash", Content: "a.go"}))
	msgs, err := Fork(events, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 6 {
		t.Fatalf("the person's call entered the conversation:\n%s", dump(msgs))
	}
}

func fullSession() []Event {
	return []Event{
		ev(1, EvUserMessage, Message{Text: "fix the bug"}),
		ev(2, EvAgentMessage, Message{Text: "Reading the file."}),
		ev(3, EvActionRequested, ActionRequested{CallID: "c1", Tool: "read",
			Args: json.RawMessage(`{"path":"a.go"}`)}),
		ev(4, EvObservation, Observation{CallID: "c1", Tool: "read", Content: "package main"}),
		ev(5, EvAgentMessage, Message{Text: "Found it."}),
		ev(6, EvActionRequested, ActionRequested{CallID: "c2", Tool: "edit",
			Args: json.RawMessage(`{"path":"a.go"}`)}),
		ev(7, EvObservation, Observation{CallID: "c2", Tool: "edit", Content: "edited"}),
		ev(8, EvAgentMessage, Message{Text: "Done."}),
	}
}

func TestForkReconstructsTheWholeConversation(t *testing.T) {
	msgs, err := Fork(fullSession(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// user, assistant+call, tool, assistant+call, tool, assistant
	if len(msgs) != 6 {
		t.Fatalf("got %d messages:\n%s", len(msgs), dump(msgs))
	}
	if msgs[0].Role != model.RoleUser || msgs[0].Content != "fix the bug" {
		t.Errorf("first message = %+v", msgs[0])
	}
	// A tool call must travel with the assistant turn that made it.
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].ID != "c1" {
		t.Errorf("assistant turn lost its tool call: %+v", msgs[1])
	}
	if msgs[2].Role != model.RoleTool || msgs[2].ToolCallID != "c1" {
		t.Errorf("tool result misplaced: %+v", msgs[2])
	}
}

// Forking mid-session keeps the work that was right and drops the rest.
func TestForkAtASequenceKeepsOnlyWhatCameBefore(t *testing.T) {
	msgs, err := Fork(fullSession(), 4) // through the first tool result
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3:\n%s", len(msgs), dump(msgs))
	}
	for _, m := range msgs {
		if m.Content == "Found it." || m.Content == "Done." {
			t.Errorf("a message from after the fork point survived: %q", m.Content)
		}
	}
}

// A call whose result was never recorded would leave the model answering a
// question that has no answer, so the incomplete exchange is dropped.
func TestForkDropsAnUnansweredToolCall(t *testing.T) {
	events := []Event{
		ev(1, EvUserMessage, Message{Text: "go"}),
		ev(2, EvAgentMessage, Message{Text: "working"}),
		ev(3, EvActionRequested, ActionRequested{CallID: "c1", Tool: "read"}),
		// no observation: the session was interrupted here
	}
	msgs, err := Fork(events, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if len(m.ToolCalls) > 0 {
			t.Fatalf("an unanswered call survived the fork:\n%s", dump(msgs))
		}
	}
}

// A tool call made with no preceding text still needs an assistant turn to
// hang from, or the message sequence is invalid.
func TestForkSynthesisesAnAssistantTurnForABareToolCall(t *testing.T) {
	events := []Event{
		ev(1, EvUserMessage, Message{Text: "go"}),
		ev(2, EvActionRequested, ActionRequested{CallID: "c1", Tool: "glob"}),
		ev(3, EvObservation, Observation{CallID: "c1", Tool: "glob", Content: "a.go"}),
	}
	msgs, err := Fork(events, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[1].Role != model.RoleAssistant || len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("want user, assistant+call, tool:\n%s", dump(msgs))
	}
}

func TestForkRejectsAnEmptyRange(t *testing.T) {
	if _, err := Fork(fullSession(), -1); err == nil {
		t.Skip("negative seq means everything by convention")
	}
	if _, err := Fork(nil, 0); err == nil {
		t.Fatal("forking from no events must be an error, not an empty session")
	}
}

func dump(msgs []model.Message) string {
	out := ""
	for i, m := range msgs {
		out += string(rune('0'+i)) + " " + string(m.Role) + " " + m.Content
		for _, c := range m.ToolCalls {
			out += " [call " + c.ID + "]"
		}
		out += "\n"
	}
	return out
}
