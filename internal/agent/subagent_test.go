package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

func subFactory(t *testing.T, turns []scriptedTurn, budget *Budget) *SubagentFactory {
	t.Helper()
	dir := tempDir(t)
	sess, err := tools.NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &SubagentFactory{
		Adapter:   &scriptedAdapter{turns: turns},
		Tools:     tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Edit{}, tools.Glob{}, tools.Grep{}),
		Policy:    policy.New(policy.ModeDefault),
		Approver:  AutoApprove{Yes: true},
		Session:   sess,
		Store:     NewMemStore(),
		Budget:    budget,
		Config:    DefaultConfig(),
		Workspace: dir,
	}
}

// The core value of delegation: the parent gets a summary, not a transcript.
func TestSubagentReturnsOnlySummary(t *testing.T) {
	f := subFactory(t, []scriptedTurn{
		{calls: []model.ToolCall{call("glob", map[string]string{"pattern": "*.go"})}},
		{calls: []model.ToolCall{call("glob", map[string]string{"pattern": "*.md"})}},
		{text: "Found 3 Go files and 2 markdown files under pkg/."},
	}, NewBudget(1_000_000, 10, false))

	summary, err := f.Spawn(context.Background(), SubagentRequest{
		Prompt: "find the files", Description: "find files", AgentType: "explore",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "Found 3 Go files") {
		t.Fatalf("summary missing: %q", summary)
	}
	// Intermediate tool traffic must not leak into what the parent sees.
	if strings.Contains(summary, "glob") {
		t.Fatalf("subagent transcript leaked to the parent: %q", summary)
	}
}

func TestSubagentBudgetExhaustionIsClean(t *testing.T) {
	b := NewBudget(100, 10, false)
	b.Spend(200) // already over

	f := subFactory(t, []scriptedTurn{{text: "done"}}, b)
	_, err := f.Spawn(context.Background(), SubagentRequest{
		Prompt: "x", Description: "y",
	})
	if err == nil {
		t.Fatal("spawn must fail once the budget is exhausted")
	}
	if !strings.Contains(err.Error(), "budget limit reached") {
		t.Fatalf("error should name the cause: %v", err)
	}
	// And it must tell the model what to do instead of retrying.
	if !strings.Contains(err.Error(), "context you have") {
		t.Fatalf("error should guide recovery: %v", err)
	}
}

func TestSubagentCountLimit(t *testing.T) {
	b := NewBudget(1_000_000, 2, false)
	f := subFactory(t, []scriptedTurn{{text: "ok"}}, b)

	for i := 0; i < 2; i++ {
		if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y"}); err != nil {
			t.Fatalf("spawn %d should succeed: %v", i, err)
		}
	}
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y"}); err == nil {
		t.Fatal("third spawn must be refused")
	}
}

func TestNestedSpawningDisabledByDefault(t *testing.T) {
	f := subFactory(t, []scriptedTurn{{text: "ok"}}, NewBudget(1_000_000, 10, false))
	f.Depth = 1 // we are already inside a subagent

	_, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y"})
	if err == nil {
		t.Fatal("nested spawn must be refused by default")
	}
	if !strings.Contains(err.Error(), "nested") {
		t.Fatalf("error should name the reason: %v", err)
	}
}

func TestSubagentSpendCountsAgainstParentBudget(t *testing.T) {
	b := NewBudget(1_000_000, 10, false)
	f := subFactory(t, []scriptedTurn{
		{calls: []model.ToolCall{call("glob", map[string]string{"pattern": "*"})}},
		{text: "done"},
	}, b)

	before := b.Spent()
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y"}); err != nil {
		t.Fatal(err)
	}
	if b.Spent() <= before {
		t.Fatal("subagent spend must count against the parent budget")
	}
}

// A read-only profile must not be able to write, or the orchestrator loses
// track of what changed.
func TestExploreProfileCannotWrite(t *testing.T) {
	f := subFactory(t, []scriptedTurn{{text: "ok"}}, NewBudget(1_000_000, 10, false))

	sub := f.Tools.Subset(Profiles["explore"].Tools...)
	for _, banned := range []string{"write", "edit", "bash"} {
		if _, found := sub.Get(banned); found {
			t.Errorf("explore profile must not expose %q", banned)
		}
	}
	for _, needed := range []string{"read", "glob", "grep"} {
		if _, found := sub.Get(needed); !found {
			t.Errorf("explore profile needs %q", needed)
		}
	}
}

func TestTaskToolRejectsEmptyPrompt(t *testing.T) {
	tool := Task{
		Spawn:    func(context.Context, SubagentRequest) (string, error) { return "", nil },
		Profiles: Profiles,
	}
	args, _ := json.Marshal(taskArgs{Description: "something"})
	res := tool.Run(context.Background(), nil, args)
	if !res.IsError || !strings.Contains(res.Content, "self-contained") {
		t.Fatalf("empty prompt must be refused with guidance: %s", res.Content)
	}
}

func TestTaskToolRejectsUnknownAgentType(t *testing.T) {
	tool := Task{
		Spawn:    func(context.Context, SubagentRequest) (string, error) { return "ok", nil },
		Profiles: Profiles,
	}
	args, _ := json.Marshal(taskArgs{Prompt: "do it", Description: "x", AgentType: "wizard"})
	res := tool.Run(context.Background(), nil, args)
	if !res.IsError || !strings.Contains(res.Content, "Available") {
		t.Fatalf("unknown type should list valid ones: %s", res.Content)
	}
}

func TestSubagentSummaryIsBounded(t *testing.T) {
	huge := strings.Repeat("x", MaxSummaryChars+5000)
	f := subFactory(t, []scriptedTurn{{text: huge}}, NewBudget(1_000_000, 10, false))

	summary, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary) > MaxSummaryChars+100 {
		t.Fatalf("summary must be bounded, got %d chars", len(summary))
	}
	if !strings.Contains(summary, "truncated") {
		t.Fatal("truncation must be visible to the parent")
	}
}

func TestSubagentEarlyTerminationIsReported(t *testing.T) {
	var turns []scriptedTurn
	for i := 0; i < 10; i++ {
		turns = append(turns, scriptedTurn{calls: []model.ToolCall{
			call("glob", map[string]string{"pattern": "*"}),
		}})
	}
	f := subFactory(t, turns, NewBudget(1_000_000, 10, false))
	f.Config.MaxTurns = 2

	summary, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "loop", Description: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "ended early") {
		t.Fatalf("parent must be told the subagent did not finish: %q", summary)
	}
}

// A subagent in its own worktree keeps the parent's syntax mode: a parent that
// turned the check off must not get a child that refuses.
func TestWorktreeSubagentKeepsTheSyntaxMode(t *testing.T) {
	child := tempDir(t)
	p := filepath.Join(child, "cfg.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := subFactory(t, []scriptedTurn{
		{calls: []model.ToolCall{call("read", map[string]string{"path": p})}},
		{calls: []model.ToolCall{call("write", map[string]string{"path": p, "content": "{"})}},
		{text: "done"},
	}, NewBudget(1_000_000, 10, false))
	f.Session.Syntax = tools.SyntaxOff
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", Workspace: child}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "{" {
		t.Fatalf("the child refused a write its parent allows: %q", got)
	}
}
