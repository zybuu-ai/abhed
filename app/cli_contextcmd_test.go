package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// charAdapter counts a token per four characters of everything it is sent.
type charAdapter struct{ window int }

func (charAdapter) Name() string { return "chars" }
func (c charAdapter) Profile() model.Profile {
	return model.Profile{Name: "chars", ContextWindow: c.window}
}
func (charAdapter) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	return nil, nil
}
func (charAdapter) CountTokens(r model.Request) (int, error) {
	n := len(r.System)
	for _, m := range r.Messages {
		n += len(m.Content)
	}
	for _, t := range r.Tools {
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	return n / 4, nil
}

// mcpTool stands for a tool an MCP server supplies.
type mcpTool struct{}

func (mcpTool) Name() string            { return "mcp__srv__lookup" }
func (mcpTool) Description() string     { return strings.Repeat("m", 400) }
func (mcpTool) Schema() json.RawMessage { return json.RawMessage(`{}`) }
func (mcpTool) Mutates() bool           { return false }
func (mcpTool) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	return tools.Result{}
}

func TestContextBreakdown(t *testing.T) {
	st, _, sf := customRig(t)
	st.adapter = charAdapter{window: 10000}
	st.loop.Tools = tools.NewRegistry(tools.Read{}, mcpTool{})
	mem := strings.Repeat("r", 800)
	write(t, st.sess.Root+"/ABHED.md", mem)
	sessionMemory.Store(agent.LoadMemory(agent.MemoryOptions{Workspace: st.sess.Root}))
	t.Cleanup(func() { sessionMemory.Store(nil) })
	st.loop.Config.SystemPrompt = strings.Repeat("s", 4000) + sessionMemory.Load().Render()
	st.loop.SetHistory([]model.Message{{Role: model.RoleUser, Content: strings.Repeat("u", 2000)}}, 1)

	parts, window := contextBreakdown(st)
	got := map[string]int{}
	for _, p := range parts {
		got[p.label] = p.tokens
	}
	if window != 10000 || got["system prompt"] != 1000 || got["memory files"] < 200 || got["memory files"] > 215 ||
		got["messages"] != 500 || got["MCP tools"] < 100 || got["tools"] == 0 {
		t.Fatalf("breakdown %v", got)
	}
	typeLine(t, st, "/context")
	out := sf.shown()
	for _, want := range []string{"system prompt", "memory files", "MCP tools", "messages", "free", "5.0%"} {
		if !strings.Contains(out, want) {
			t.Fatalf("/context lacks %q:\n%s", want, out)
		}
	}
}

// End to end: /context after a message.
func TestCLIContext(t *testing.T) {
	c := startCLI(t)
	c.task("hello")
	c.command("/context", "of window")
	if !strings.Contains(c.out.String(), "messages") || !strings.Contains(c.out.String(), "8192") {
		t.Fatalf("/context:\n%s", c.out.String())
	}
}
