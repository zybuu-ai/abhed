package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
)

// A greeting is no claim about the session: strict mode completes it in one
// call, with nothing counted as work and no revision asked for.
func TestGreetingIsNotSessionWork(t *testing.T) {
	l, store, ad, _ := verifyHarness(t, VerifyStrict, false, []scriptedTurn{
		{text: "Hi! I'm Abhed, your technical assistant. I can help with code, infrastructure and questions about your workspace."},
	})
	reason, _ := l.Run(context.Background(), "hi")
	if reason != TermCompleted || len(ad.gotRequests) != 1 {
		t.Fatalf("want completed in one call, got %s after %d calls", reason, len(ad.gotRequests))
	}
	vs := answerVerdicts(store)
	if len(vs) != 1 || vs[0].Outcome != ClaimVerified || vs[0].Revision != "" {
		t.Fatalf("verdicts: %+v", vs)
	}
	for _, c := range vs[0].Claims {
		if c.LoadBearing || c.Kind == KindObserved {
			t.Fatalf("counted as the session's work: %+v", c)
		}
	}
}

// Given through the answer tool as observed, an introduction is still no
// work; a claim beside it is checked as before.
func TestGreetingThroughTheAnswerTool(t *testing.T) {
	l, store, _, _ := verifyHarness(t, VerifyStrict, true, []scriptedTurn{
		{calls: []model.ToolCall{{ID: "a1", Name: "answer", Args: json.RawMessage(
			`{"text":"I'm Abhed, your technical assistant.","claims":[{"text":"I'm Abhed, your technical assistant","kind":"observed"}]}`)}}},
	})
	if reason, _ := l.Run(context.Background(), "who are you"); reason != TermCompleted {
		t.Fatalf("want completed, got %s", reason)
	}
	vs := answerVerdicts(store)
	if len(vs) != 1 || vs[0].Outcome != ClaimVerified {
		t.Fatalf("verdicts: %+v", vs)
	}
	for _, c := range vs[0].Claims {
		if c.LoadBearing {
			t.Fatalf("counted as work: %+v", c)
		}
	}
}

// What reports the session is never read as an introduction.
func TestSelfDescriptionKeepsWork(t *testing.T) {
	for _, s := range []string{
		"I can confirm the tests pass.", "I'm Abhed, and all 45 tests passed.", "I can help, and I have already fixed the login bug.",
		"I'm your assistant and I updated config/settings.yaml.", "I can run `make lint` and it was clean.", "I'm done: the build is green.",
		"I can see 3 replicas in the deployment.", "I'm Abhed; main.go:3 defines Serve.", "I am able to reach db.prod.acme.example now.",
		"I'm Abhed, and the server is healthy.", "I can help: the login bug is resolved.", "I'm Abhed, and your workspace is secure.",
		"I'm Abhed; the service is up.", "I'm your assistant, and the code is correct.", "I am your assistant; the bug is gone.",
		"I'm your assistant, and the migration is safe to run in production.", "Hi, I'm Abhed and the deploy is live.",
	} {
		if selfDescription(Claim{Text: s}) {
			t.Errorf("read as an introduction: %q", s)
		}
	}
	for _, s := range []string{"I'm Abhed, your technical assistant.", "Hello there, I'm here to help with your project.", "I can read files, run commands in a sandbox and edit code."} {
		if !selfDescription(Claim{Text: s}) {
			t.Errorf("not read as an introduction: %q", s)
		}
		if selfDescription(Claim{Text: s, Evidence: []string{"#2"}}) {
			t.Errorf("a cited sentence read as an introduction: %q", s)
		}
	}
}
