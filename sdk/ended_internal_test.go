package abhed

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/extension"
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

// A user_prompt_submit hook among the extensions refuses an embedded run's
// prompt: it ends as prompt_refused and the model is never called.
func TestSDKPromptHookRefusesAPrompt(t *testing.T) {
	a, err := New(context.Background(), Options{
		Workspace: t.TempDir(),
		Provider:  &Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"}, // never reached
		Extensions: []ExtensionConfig{{Name: "gatekeeper", Command: "bash",
			Args:   []string{filepath.Join("..", "internal", "extension", "testdata", "gatekeeper.sh")},
			Events: []extension.Event{extension.EvUserPromptSubmit}, Timeout: 5 * time.Second}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	_, err = a.Run(context.Background(), "the forbidden thing")
	var ended *EndedError
	if !errors.As(err, &ended) || ended.Reason != agent.TermPromptRefused {
		t.Fatalf("Run: %v", err)
	}
}
