package abhed

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// A run that ended early says why as a value, with the message it always had.
func TestEndedErrorCarriesTheReason(t *testing.T) {
	var err error = &EndedError{Reason: agent.TermMaxTurns}
	var ended *EndedError
	if !errors.As(err, &ended) || ended.Reason != agent.TermMaxTurns || err.Error() != "abhed: ended as max_turns" {
		t.Fatalf("%v", err)
	}
}

// refuseAll is a user_prompt_submit hook that refuses every message.
type refuseAll struct{}

func (refuseAll) PromptSubmitted(context.Context, string, string) string { return "not today" }
func (refuseAll) PermissionRequested(context.Context, *policy.Engine, string, string, json.RawMessage, string) string {
	return ""
}
func (refuseAll) Observe(context.Context, string, string, string, string) {}

// RunQueued with every queued message refused calls no model and ends as
// prompt_refused, a value the caller can read.
func TestRunQueuedRefusedEndsAsPromptRefused(t *testing.T) {
	a := forwardingAgent(t, nil)
	a.loop.Hooks = refuseAll{}
	a.Steer("something the hook refuses")
	_, err := a.RunQueued(context.Background())
	var ended *EndedError
	if !errors.As(err, &ended) || ended.Reason != agent.TermPromptRefused {
		t.Fatalf("RunQueued: %v", err)
	}
}
