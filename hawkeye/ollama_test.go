package hawkeye

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A person's start, install or pull of Ollama is reported, with how it ended.
func TestLocalModelStepsAreReported(t *testing.T) {
	r := (&rec{}).add(agent.EvOllamaStart, agent.ActorUser, agent.Trusted, agent.OllamaAction{
		Op: "start", Outcome: agent.OllamaOK, Provider: "local", Model: "qwen3:8b", Command: "open -a Ollama", By: agent.ByUser, Surface: "tui"})
	r.add(agent.EvOllamaPull, agent.ActorUser, agent.Trusted, agent.OllamaAction{
		Op: "pull", Outcome: agent.OllamaFailed, Model: "qwen3:8b", Command: "ollama pull qwen3:8b", Error: "no space left", By: agent.ByUser})
	r.user("x")
	got := Analyze("s-ollama", r.end(agent.TermCompleted).evs)
	if len(got.LocalModel) != 2 || got.LocalModel[0].Op != "start" || got.LocalModel[1].Outcome != agent.OllamaFailed {
		t.Fatalf("local model = %+v", got.LocalModel)
	}
	text := Text(got)
	if !strings.Contains(text, "ollama    #1 start qwen3:8b: ok (open -a Ollama)") || !strings.Contains(text, "pull qwen3:8b: failed") {
		t.Fatalf("text report:\n%s", text)
	}
	page, err := HTML(got)
	if err != nil || !strings.Contains(page, "Local model (Ollama)") || !strings.Contains(page, "no space left") {
		t.Fatalf("html report (%v) lacks the steps", err)
	}
}
