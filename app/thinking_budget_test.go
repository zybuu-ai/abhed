package app

import (
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
)

// The configured thinking budget is seen through the wrappers a session's
// adapter is built with, as cli_run builds it, so the reasoning bound is
// raised to it rather than cutting the model a quarter of the way in.
func TestThinkingBudgetThroughTheSessionAdapter(t *testing.T) {
	budget := 32000
	p := config.ProviderConfig{Type: "anthropic", Model: "claude-sonnet-4-5", APIKey: "k", BaseURL: "http://127.0.0.1:1",
		Params: config.ParamsConfig{ThinkingBudget: &budget}}
	var a model.Adapter = gatedAdapter{Adapter: buildAdapter(p), probe: newEndpointProbe(p)}
	if got := model.ThinkingBudget(a, model.Request{}); got != 32000 {
		t.Fatalf("through gatedAdapter %d", got)
	}
	f := newFallbackAdapter("a", a, []string{"b"}, func(string) (model.Adapter, error) { return &fakeAdapter{name: "B"}, nil })
	if got := model.ThinkingBudget(f, model.Request{}); got != 32000 {
		t.Fatalf("through fallbackAdapter %d", got)
	}
}

// -p's result is a continued reply whole: the part cut and its continuation.
func TestRunAnswerJoinsAContinuedReply(t *testing.T) {
	msgs := []model.Message{
		{Role: model.RoleUser, Content: "go"},
		{Role: model.RoleAssistant, Content: "The answer is"},
		{Role: model.RoleUser, Content: agent.ContinueCutReply},
		{Role: model.RoleAssistant, Content: " 42."},
	}
	if got := runAnswer(msgs, nil, "s", 0); got != "The answer is 42." {
		t.Fatalf("result %q", got)
	}
	if got := runAnswer(msgs[:2], nil, "s", 0); got != "The answer is" {
		t.Fatalf("an uncontinued reply %q", got)
	}
}
