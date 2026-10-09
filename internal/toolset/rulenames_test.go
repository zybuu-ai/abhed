package toolset

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/index"
	"github.com/zybuu-ai/abhed/internal/k8s"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/remote"
	"github.com/zybuu-ai/abhed/internal/skills"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/webfetch"
	"github.com/zybuu-ai/abhed/internal/websearch"
)

// Every tool Abhed builds is a built-in name, so a rule naming it in another
// case is read as naming it; a tool left off the list would not be.
func TestEveryBuiltinToolIsABuiltinName(t *testing.T) {
	for _, tl := range []tools.Tool{
		tools.Read{}, tools.Write{}, tools.Edit{}, tools.Glob{}, tools.Grep{}, tools.Bash{},
		tools.Todo{}, &tools.Ask{}, &tools.MemoryWrite{}, tools.ExitPlan{},
		agent.ShellOutput{}, agent.ShellKill{}, agent.TodoTool{}, agent.Task{}, agent.Tasks{},
		agent.TaskStatus{}, agent.TaskCancel{}, agent.Recall{}, &ToolSearch{},
		&websearch.Tool{}, &webfetch.Tool{}, &index.SearchTool{},
		k8s.GetTool{}, k8s.ApplyTool{}, k8s.LoginTool{}, remote.Tool{}, remote.ConnectTool{}, skills.Tool{},
	} {
		if !policy.BuiltinTool(tl.Name()) {
			t.Errorf("%s is not among the built-in names a rule folds to", tl.Name())
		}
	}
	if !policy.BuiltinTool(agent.ResultToolName) {
		t.Errorf("%s is not a built-in name", agent.ResultToolName)
	}
}

// A rule whose tool differs from a tool in the set only in case, an MCP or
// extension tool compared exactly, is named; one that matches is not.
func TestCaseOnlyRules(t *testing.T) {
	cfg := config.Default()
	cfg.Permissions.Deny = []string{"mcp__github__Create_Issue", "mcp__github__list", "Bash(curl *)"}
	got := CaseOnlyRules(cfg, []string{"bash", "mcp__github__create_issue", "mcp__github__list"})
	if len(got) != 1 || !containsAll(got[0], "mcp__github__Create_Issue", "mcp__github__create_issue") {
		t.Fatalf("got %q", got)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func TestCaseOnlyRulesForServerWildcards(t *testing.T) {
	cfg := config.Default()
	cfg.Permissions.Deny = []string{"mcp__GitHub__*", "mcp__github__*"}
	got := CaseOnlyRules(cfg, []string{"mcp__github__create_issue"})
	if len(got) != 1 || !strings.Contains(got[0], "mcp__GitHub__*") {
		t.Fatalf("got %q", got)
	}
}
