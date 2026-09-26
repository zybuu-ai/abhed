package abhed

import (
	"context"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// An embedder's NoteAnswer reaches the loop with what it said, and nothing it could not say.
func TestNoteAnswerReachesTheLoop(t *testing.T) {
	ctx, got := agent.ExpectAnswer(context.Background())
	NoteAnswer(ctx, Answer{By: BySessionScope, Scope: "bash(go test *)"})
	if got.By != agent.BySessionScope || got.Scope != "bash(go test *)" || got.Held {
		t.Fatalf("answer %+v", got)
	}
	ctx, got = agent.ExpectAnswer(context.Background())
	NoteAnswer(ctx, Answer{By: "someone"})
	if got.By != "" {
		t.Fatalf("an unknown By was kept: %+v", got)
	}
}
