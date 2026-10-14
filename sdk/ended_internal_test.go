package abhed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	if !errors.As(err, &ended) || ended.Reason != agent.TermPromptRefused || ended.Detail != "that topic is off limits" {
		t.Fatalf("Run: %v", err)
	}
}

// A refused prompt with nothing queued before it returns no answer: not the
// previous prompt's, which a caller would take for this one's.
func TestRefusedPromptReturnsNoEarlierAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	a, err := New(context.Background(), Options{
		Workspace: t.TempDir(),
		Provider:  &Provider{Type: "openai-compatible", BaseURL: srv.URL, Model: "m"},
		Extensions: []ExtensionConfig{{Name: "gatekeeper", Command: "bash",
			Args:   []string{filepath.Join("..", "internal", "extension", "testdata", "gatekeeper.sh")},
			Events: []extension.Event{extension.EvUserPromptSubmit}, Timeout: 5 * time.Second}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if got, err := a.Run(context.Background(), "hello"); err != nil || got != "ok" {
		t.Fatalf("first prompt: %q %v", got, err)
	}
	got, err := a.Run(context.Background(), "the forbidden thing")
	var ended *EndedError
	if got != "" || !errors.As(err, &ended) || ended.Reason != agent.TermPromptRefused {
		t.Fatalf("refused prompt: %q %v", got, err)
	}
	// RunQueued with every queued message refused answers nothing either.
	a.Steer("more forbidden talk")
	got, err = a.RunQueued(context.Background())
	if got != "" || !errors.As(err, &ended) || ended.Reason != agent.TermPromptRefused {
		t.Fatalf("RunQueued, every message refused: %q %v", got, err)
	}
}
