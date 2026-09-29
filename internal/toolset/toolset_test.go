package toolset

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/skills"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// scripted calls the skill on its first turn, answers a pipeline's model step
// with "classified", and keeps every prompt it was sent.
type scripted struct {
	mu      sync.Mutex
	turn    int
	prompts []string
}

func (*scripted) Name() string { return "scripted" }
func (*scripted) Profile() model.Profile {
	return model.Profile{Name: "scripted", ContextWindow: 100000}
}
func (*scripted) CountTokens(model.Request) (int, error) { return 0, nil }
func (s *scripted) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan model.Chunk, 3)
	defer close(ch)
	if len(req.Tools) == 0 { // a pipeline's model step is offered no tools
		s.prompts = append(s.prompts, req.Messages[0].Content)
		ch <- model.Chunk{Type: model.ChunkText, Text: "classified"}
	} else if s.turn++; s.turn == 1 {
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &model.ToolCall{ID: "c1", Name: "skill", Args: json.RawMessage(`{"name":"gather"}`)}}
	} else {
		ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
	return ch, nil
}

// A shared skill tool's pipeline takes its input and its model from the loop
// that called it, so one tool serves every session without a global.
func TestPipelineTakesTheCallingLoopsRequestAndModel(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gather")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: gather\ndescription: use when gathering\n---\nWrite the answer.\n"
	pipe := `{"stages":[{"name":"classify","steps":[{"kind":"model","prompt":"classify: {{input}}","output":"kind","required":true}]}]}`
	for name, body := range map[string]string{"SKILL.md": md, "pipeline.json": pipe} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg, errs := skills.Load([]string{root})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	shared := tools.NewRegistry(SkillTool(reg))
	for _, question := range []string{"first question", "second question"} {
		m := &scripted{}
		sess, err := tools.NewSession(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		l := agent.NewLoop(m, shared, policy.New(policy.ModeDefault), agent.AutoApprove{}, sess,
			agent.NewRecorder(agent.NewMemStore(), "s", ""), agent.DefaultConfig())
		if _, err := l.Run(context.Background(), question); err != nil {
			t.Fatal(err)
		}
		if len(m.prompts) != 1 || !strings.Contains(m.prompts[0], question) {
			t.Fatalf("the pipeline for %q was sent %v", question, m.prompts)
		}
	}
}

// Binding one session's subagents leaves the shared registry without them.
func TestSubagentsLeaveTheSharedRegistryAlone(t *testing.T) {
	shared := tools.NewRegistry(tools.Read{})
	f := &agent.SubagentFactory{Workspace: t.TempDir(), Budget: Budget(config.Default())}
	own := Subagents(shared, f, 2)
	if _, ok := shared.Get("task"); ok {
		t.Fatal("the shared registry was given a session's task tool")
	}
	if _, ok := own.Get("tasks"); !ok || f.Tools != own {
		t.Fatal("the session's registry lacks its subagent tools, or the factory spawns from another")
	}
	if tk, _ := own.Get("tasks"); tk.(agent.Tasks).MaxParallel != 2 || tk.(agent.Tasks).Workspace != f.Workspace {
		t.Fatalf("tasks is not bound to the session: %+v", tk)
	}
}
