package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
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
// question that has no answer, so it is answered: it may have run.
func TestForkAnswersAnUnansweredToolCall(t *testing.T) {
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
	if last := msgs[len(msgs)-1]; last.Role != model.RoleTool || last.ToolCallID != "c1" || last.Content != "No result was recorded; it may have run." {
		t.Fatalf("the unanswered call was not answered:\n%s", dump(msgs))
	}
}

// A fork cut inside a turn drops that turn: its results lie past the cut.
func TestForkInsideATurnDropsIt(t *testing.T) {
	events := []Event{
		ev(1, EvUserMessage, Message{Text: "go"}),
		ev(2, EvAgentMessage, Message{Text: "working"}),
		ev(3, EvActionRequested, ActionRequested{CallID: "c1", Tool: "read"}),
		ev(4, EvObservation, Observation{CallID: "c1", Content: "x"}),
	}
	msgs, err := Fork(events, 3)
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

// ForkTo(0) keeps the whole conversation, as Fork(0) did; a step past the end
// or one an earlier fork abandoned is refused.
func TestForkToZeroKeepsAllAndRefusesAbandonedSteps(t *testing.T) {
	events := []Event{
		ev(1, EvUserMessage, Message{Text: "one"}),
		ev(2, EvAgentMessage, Message{Text: "ok"}),
		ev(3, EvUserMessage, Message{Text: "two"}),
		ev(4, EvForked, Forked{ThroughSeq: 2}),
		ev(5, EvUserMessage, Message{Text: "three"}),
	}
	sess, _ := tools.NewSession(tempDir(t))
	l := NewLoop(nil, nil, nil, nil, sess, NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
	l.Recorder.Advance(5)
	if n, err := l.ForkTo(events, 0); err != nil || n != 3 {
		t.Fatalf("ForkTo(0) = %d, %v; want the 3 live messages", n, err)
	}
	for _, seq := range []int64{3, 99} {
		if _, err := l.ForkTo(events, seq); err == nil {
			t.Errorf("ForkTo(%d) was taken", seq)
		}
	}
}

// A fork after a turn with a refused call keeps that turn: a refusal settles
// its call, so the fork is not taken for a cut inside the turn.
func TestForkAfterADeniedTurnKeepsIt(t *testing.T) {
	events := []Event{
		ev(1, EvUserMessage, Message{Text: "one"}),
		ev(2, EvModelCall, ModelCall{Turn: 1}),
		ev(3, EvActionRequested, ActionRequested{CallID: "a", Tool: "nope"}),
		ev(4, EvActionDenied, map[string]string{"call_id": "a", "reason": "unknown tool \"nope\""}),
		ev(5, EvModelCall, ModelCall{Turn: 2}),
		ev(6, EvAgentMessage, Message{Text: "answer one"}),
		ev(7, EvSessionEnded, SessionEnded{Reason: TermCompleted}),
		ev(8, EvUserMessage, Message{Text: "two"}),
		ev(9, EvSessionEnded, SessionEnded{Reason: TermCompleted}),
	}
	msgs, err := Fork(events, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := dump(msgs); !strings.Contains(got, "answer one") || !strings.Contains(got, "Not run: denied — unknown tool") {
		t.Fatalf("the fork lost the turn with the refused call:\n%s", got)
	}
}

// Two model turns stay two assistant messages when the first one's calls were
// all refused, and each turn's results follow its calls in call order.
func TestForkKeepsTurnsApartAndResultsInOrder(t *testing.T) {
	events := []Event{
		ev(1, EvUserMessage, Message{Text: "go"}),
		ev(2, EvModelCall, ModelCall{Turn: 1}),
		ev(3, EvActionRequested, ActionRequested{CallID: "a", Tool: "write"}),
		ev(4, EvActionDenied, map[string]string{"call_id": "a", "reason": "rejected"}),
		ev(5, EvModelCall, ModelCall{Turn: 2}),
		ev(6, EvActionRequested, ActionRequested{CallID: "b", Tool: "write"}),
		ev(7, EvActionRequested, ActionRequested{CallID: "c", Tool: "read"}),
		ev(8, EvActionDenied, map[string]string{"call_id": "b", "reason": "rejected"}),
		ev(9, EvObservation, Observation{CallID: "c", Content: "text"}),
	}
	msgs, err := Fork(events, 0)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	turns := 0
	for _, m := range msgs {
		if len(m.ToolCalls) > 0 {
			turns++
		}
		if m.Role == model.RoleTool {
			order = append(order, m.ToolCallID)
		}
	}
	if turns != 2 || strings.Join(order, ",") != "a,b,c" {
		t.Fatalf("%d assistant turns, results %v; want 2 and a,b,c:\n%s", turns, order, dump(msgs))
	}
}

// A call that never got a result early on (a crash, or a record from before
// not_run), then later tasks: a fork near the end keeps the later tasks.
func TestForkKeepsLaterTasksAfterAnEarlyUnansweredCall(t *testing.T) {
	events := []Event{
		ev(1, EvUserMessage, Message{Text: "one"}),
		ev(2, EvModelCall, ModelCall{}),
		ev(3, EvActionRequested, ActionRequested{CallID: "c1", Tool: "glob"}),
		ev(4, EvActionApproved, map[string]string{"call_id": "c1"}),
		ev(5, EvSessionEnded, SessionEnded{Reason: TermShutdown}),
		ev(6, EvUserMessage, Message{Text: "two"}),
		ev(7, EvModelCall, ModelCall{}),
		ev(8, EvAgentMessage, Message{Text: "answer two"}),
		ev(9, EvSessionEnded, SessionEnded{Reason: TermCompleted}),
		ev(10, EvUserMessage, Message{Text: "three"}),
		ev(11, EvModelCall, ModelCall{}),
		ev(12, EvAgentMessage, Message{Text: "answer three"}),
		ev(13, EvSessionEnded, SessionEnded{Reason: TermCompleted}),
	}
	msgs, err := Fork(events, 9)
	if err != nil {
		t.Fatal(err)
	}
	if got := dump(msgs); !strings.Contains(got, "answer two") || !strings.Contains(got, "No result was recorded") {
		t.Fatalf("the fork dropped a later task behind an early unanswered call:\n%s", got)
	}
}

// With an early unanswered call, a fork after a later tool turn keeps that
// turn and its result; only a turn whose result lies past the cut is dropped.
func TestForkKeepsALaterToolTurnAfterAnEarlyOrphan(t *testing.T) {
	events := []Event{
		ev(1, EvUserMessage, Message{Text: "one"}),
		ev(2, EvModelCall, ModelCall{}),
		ev(3, EvActionRequested, ActionRequested{CallID: "c1", Tool: "glob"}),
		ev(4, EvSessionEnded, SessionEnded{Reason: TermShutdown}),
		ev(5, EvUserMessage, Message{Text: "two"}),
		ev(6, EvModelCall, ModelCall{}),
		ev(7, EvActionRequested, ActionRequested{CallID: "c2", Tool: "glob"}),
		ev(8, EvObservation, Observation{CallID: "c2", Content: "found"}),
		ev(9, EvModelCall, ModelCall{}),
		ev(10, EvAgentMessage, Message{Text: "answer two"}),
		ev(11, EvUserMessage, Message{Text: "three"}),
		ev(12, EvModelCall, ModelCall{}),
		ev(13, EvActionRequested, ActionRequested{CallID: "c3", Tool: "glob"}),
		ev(14, EvObservation, Observation{CallID: "c3", Content: "later"}),
	}
	msgs, err := Fork(events, 13)
	if err != nil {
		t.Fatal(err)
	}
	got := dump(msgs)
	if !strings.Contains(got, "found") || !strings.Contains(got, "answer two") || !strings.Contains(got, "three") {
		t.Fatalf("the fork lost task two behind an early orphan:\n%s", got)
	}
	if strings.Contains(got, "c3") {
		t.Fatalf("the turn cut at step 13, whose result is past the cut, was kept:\n%s", got)
	}
}

// A rebuilt request carries one result per call, only for that turn's calls:
// a result for an unknown id, a second one, or one recorded after the next
// model call is dropped, as providers refuse orphan and duplicate results.
func TestForkDropsOrphanAndDuplicateResults(t *testing.T) {
	msgs := answerAll([]model.Message{
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "a"}}},
		{Role: model.RoleTool, ToolCallID: "x", Content: "stray"},
		{Role: model.RoleTool, ToolCallID: "a", Content: "ok"},
		{Role: model.RoleTool, ToolCallID: "a", Content: "again"},
	}, nil)
	if got := dump(msgs); len(msgs) != 2 || msgs[1].ToolCallID != "a" || msgs[1].Content != "ok" {
		t.Fatalf("orphan or duplicate result kept:\n%s", got)
	}

	if lone := answerAll([]model.Message{{Role: model.RoleUser, Content: "go"}, {Role: model.RoleTool, ToolCallID: "x"}}, nil); len(lone) != 1 {
		t.Fatalf("a result with no turn before it was kept:\n%s", dump(lone))
	}

	late, err := Fork([]Event{
		ev(1, EvUserMessage, Message{Text: "go"}),
		ev(2, EvModelCall, ModelCall{}),
		ev(3, EvActionRequested, ActionRequested{CallID: "a", Tool: "glob"}),
		ev(4, EvModelCall, ModelCall{}),
		ev(5, EvActionRequested, ActionRequested{CallID: "b", Tool: "glob"}),
		ev(6, EvObservation, Observation{CallID: "a", Content: "late"}),
		ev(7, EvObservation, Observation{CallID: "b", Content: "fine"}),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for i, m := range late {
		if m.Role == model.RoleTool {
			seen[m.ToolCallID]++
			if prev := late[i-1]; prev.Role != model.RoleTool && !hasCall(prev, m.ToolCallID) {
				t.Fatalf("result %s does not follow its call:\n%s", m.ToolCallID, dump(late))
			}
		}
	}
	if seen["a"] != 1 || seen["b"] != 1 || strings.Contains(dump(late), "late") {
		t.Fatalf("a result recorded after the next model call was kept in the wrong turn:\n%s", dump(late))
	}
}

func hasCall(m model.Message, id string) bool {
	for _, c := range m.ToolCalls {
		if c.ID == id {
			return true
		}
	}
	return false
}

// A fork between a call and its refusal, recorded past the cut, drops the turn.
func TestForkBeforeADenialPastTheCutDropsTheTurn(t *testing.T) {
	events := []Event{
		ev(1, EvUserMessage, Message{Text: "one"}),
		ev(2, EvModelCall, ModelCall{}),
		ev(3, EvActionRequested, ActionRequested{CallID: "c1", Tool: "write"}),
		ev(4, EvActionDenied, map[string]string{"call_id": "c1", "reason": "no"}),
		ev(5, EvSessionEnded, SessionEnded{Reason: TermCompleted}),
	}
	msgs, err := Fork(events, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("a fork between a call and its refusal kept:\n%s", dump(msgs))
	}
}
