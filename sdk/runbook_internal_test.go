package abhed

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Options.Runbook's steps go with the first run, whichever entry point
// starts it, and only once.
func TestRunbookNoteGoesOnce(t *testing.T) {
	a := &Agent{runbookNote: "the steps"}
	if got, _ := a.withRunbook("go"); got != "go\n\nthe steps" {
		t.Fatalf("first: %q", got)
	}
	if got, _ := a.withRunbook("again"); got != "again" {
		t.Fatalf("second: %q", got)
	}
}

// A run whose prompt a hook refused never sent the steps, so the next run
// carries them.
func TestRunbookNoteOutlivesARefusedPrompt(t *testing.T) {
	a := forwardingAgent(t, nil)
	a.runbookNote = "the steps"
	a.loop.Hooks = refuseAll{}
	_, err := a.Run(context.Background(), "go")
	var ended *EndedError
	if !errors.As(err, &ended) || ended.Reason != agent.TermPromptRefused {
		t.Fatalf("Run: %v", err)
	}
	if got, _ := a.withRunbook("again"); !strings.Contains(got, "the steps") {
		t.Fatalf("the steps were lost with the refused prompt: %q", got)
	}
}
