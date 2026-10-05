package app

import (
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
)

// A continued run's result line is its own answer: one stopped before
// answering says nothing, rather than repeat the session's earlier answer.
func TestResultIsThisRunsAnswer(t *testing.T) {
	ms := agent.NewMemStore()
	_ = ms.Append(agent.Event{ID: "e1", SessionID: "s", Seq: 1, Type: agent.EvUserMessage, Payload: []byte(`{"text":"q"}`)})
	_ = ms.Append(agent.Event{ID: "e2", SessionID: "s", Seq: 2, Type: agent.EvAgentMessage, Payload: []byte(`{"text":"old answer"}`)})
	msgs := []model.Message{{Role: model.RoleUser, Content: "q"}, {Role: model.RoleAssistant, Content: "old answer"}}
	if got := runAnswer(msgs, ms, "s", 2); got != "" {
		t.Fatalf("an unanswered continued run reported %q", got)
	}
	if got := runAnswer(msgs, ms, "s", 0); got != "old answer" {
		t.Fatalf("a new session's answer: %q", got)
	}
	_ = ms.Append(agent.Event{ID: "e3", SessionID: "s", Seq: 3, Type: agent.EvAgentMessage, Payload: []byte(`{"text":"new"}`)})
	msgs = append(msgs, model.Message{Role: model.RoleAssistant, Content: "new"})
	if got := runAnswer(msgs, ms, "s", 2); got != "new" {
		t.Fatalf("this run's answer: %q", got)
	}
}
