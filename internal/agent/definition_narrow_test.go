package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// fakeSkills is a skill tool holding some skills, cut as the real one is.
type fakeSkills struct{ have []string }

func (fakeSkills) Name() string            { return "skill" }
func (f fakeSkills) Description() string   { return "skills: " + strings.Join(f.have, ",") }
func (fakeSkills) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (fakeSkills) Mutates() bool           { return false }
func (fakeSkills) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	return tools.Result{}
}
func (f fakeSkills) NarrowSkills(names []string) (tools.Tool, []string) {
	var kept, missing []string
	for _, n := range names {
		if slices.Contains(f.have, n) {
			kept = append(kept, n)
		} else {
			missing = append(missing, n)
		}
	}
	return fakeSkills{have: kept}, missing
}

// mcp_servers keeps only the listed servers' tools; a server the session
// has no tools from refuses the spawn.
func TestDefinitionMCPServersOnlyNarrow(t *testing.T) {
	f, ad := defFactory(t, []scriptedTurn{{text: "ok"}}, &Definition{Name: "docs", Description: "d", Instruction: "i",
		MCPServers: []string{"docs"}})
	f.Tools.Add(fakeMCP{"mcp__docs__search"})
	f.Tools.Add(fakeMCP{"mcp__other__x"})
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "docs"}); err != nil {
		t.Fatal(err)
	}
	got := toolNames(ad.gotRequests[0])
	if !slices.Contains(got, "mcp__docs__search") || slices.Contains(got, "mcp__other__x") || !slices.Contains(got, "read") {
		t.Fatalf("the child's tools: %v", got)
	}

	f, _ = defFactory(t, nil, &Definition{Name: "ghost", Description: "d", Instruction: "i", MCPServers: []string{"absent"}})
	f.Tools.Add(fakeMCP{"mcp__docs__search"})
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "ghost"}); err == nil ||
		!strings.Contains(err.Error(), "absent") {
		t.Fatalf("a server the session lacks: %v", err)
	}

	// An empty list keeps no MCP tools at all.
	f, ad = defFactory(t, []scriptedTurn{{text: "ok"}}, &Definition{Name: "none", Description: "d", Instruction: "i", MCPServers: []string{}})
	f.Tools.Add(fakeMCP{"mcp__docs__search"})
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "none"}); err != nil {
		t.Fatal(err)
	}
	if got := toolNames(ad.gotRequests[0]); slices.Contains(got, "mcp__docs__search") {
		t.Fatalf("an empty mcp_servers kept %v", got)
	}
}

// skills cuts the skill tool to the listed skills; one the session lacks
// refuses the spawn, and an empty list takes the tool away.
func TestDefinitionSkillsOnlyNarrow(t *testing.T) {
	f, ad := defFactory(t, []scriptedTurn{{text: "ok"}}, &Definition{Name: "pdf", Description: "d", Instruction: "i", Skills: []string{"pdf"}})
	f.Tools.Add(fakeSkills{have: []string{"pdf", "deploy"}})
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "pdf"}); err != nil {
		t.Fatal(err)
	}
	var desc string
	for _, d := range ad.gotRequests[0].Tools {
		if d.Name == "skill" {
			desc = d.Description
		}
	}
	if desc != "skills: pdf" {
		t.Fatalf("the child's skill tool: %q", desc)
	}

	f, _ = defFactory(t, nil, &Definition{Name: "more", Description: "d", Instruction: "i", Skills: []string{"pdf", "secret-sauce"}})
	f.Tools.Add(fakeSkills{have: []string{"pdf"}})
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "more"}); err == nil ||
		!strings.Contains(err.Error(), "secret-sauce") {
		t.Fatalf("a skill the session lacks: %v", err)
	}

	f, ad = defFactory(t, []scriptedTurn{{text: "ok"}}, &Definition{Name: "bare", Description: "d", Instruction: "i", Skills: []string{}})
	f.Tools.Add(fakeSkills{have: []string{"pdf"}})
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "bare"}); err != nil {
		t.Fatal(err)
	}
	if got := toolNames(ad.gotRequests[0]); slices.Contains(got, "skill") {
		t.Fatalf("an empty skills list kept the skill tool: %v", got)
	}
}

// effort lowers the child's effort, never raises it above the session's.
func TestDefinitionEffortOnlyLowers(t *testing.T) {
	for _, c := range []struct {
		session model.EffortLevel
		role    string
		want    model.EffortLevel
	}{
		{model.EffortHigh, "low", model.EffortLow},
		{model.EffortLow, "high", model.EffortLow},
		{model.EffortMedium, "", model.EffortMedium},
		{model.EffortNone, "medium", model.EffortMedium},
	} {
		if got := childEffort(c.session, c.role); got != c.want {
			t.Fatalf("session %q role %q: %q, want %q", c.session, c.role, got, c.want)
		}
	}
	f, ad := defFactory(t, []scriptedTurn{{text: "ok"}}, &Definition{Name: "quick", Description: "d", Instruction: "i", Effort: "low"})
	f.Config.Effort = model.EffortHigh
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "quick"}); err != nil {
		t.Fatal(err)
	}
	if got := ad.gotRequests[0].Effort; got != model.EffortLow {
		t.Fatalf("the child ran at %q", got)
	}
}

// background holds a role to the background, or out of it.
func TestDefinitionBackground(t *testing.T) {
	yes, no := true, false
	f, _ := defFactory(t, nil, &Definition{Name: "bg", Description: "d", Instruction: "i", Background: &yes})
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "bg"}); err == nil ||
		!strings.Contains(err.Error(), "only in the background") {
		t.Fatalf("a background-only role ran in the foreground: %v", err)
	}
	f, _ = defFactory(t, nil, &Definition{Name: "fg", Description: "d", Instruction: "i", Background: &no})
	if _, err := f.prepare(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "fg"},
		map[string]any{"background": true}, nil); err == nil || !strings.Contains(err.Error(), "does not run in the background") {
		t.Fatalf("a foreground-only role ran in the background: %v", err)
	}
	if n := f.Budget.spawned.Load(); n != 0 {
		t.Fatalf("a refused spawn was counted: %d", n)
	}
}
