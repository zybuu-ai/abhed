package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/agents", Help: "the subagent types the task tool can start, by source", Group: "status", Order: 140, ReadOnly: true, Run: slashAgents})
	registerSlash(slashCmd{Name: "/skills", Help: "the skills the agent can load", Group: "status", Order: 141, ReadOnly: true, Run: slashSkills})
	registerSlash(slashCmd{Name: "/mcp", Args: "[restart <server>]", Help: "MCP servers, their state and tools; restart reconnects one", Group: "status", Order: 142, Run: slashMCP})
	registerSlash(slashCmd{Name: "/tools", Help: "every tool the agent has, and where it came from", Group: "status", Order: 143, ReadOnly: true, Run: slashTools})
}

// slashAgents is /agents: each definition with its source, model and tools.
func slashAgents(ctx context.Context, e *cmdEnv, _ []string) (bool, error) {
	st := e.st
	if st.set == nil {
		return false, fmt.Errorf("no tool set in this session")
	}
	rows := [][]string{{"type", "source", "model", "tools", "use"}}
	for _, name := range st.set.Agents.Names() {
		d, _ := st.set.Agents.Get(name)
		model := orDefault(d.Model, "the session's")
		tl := "all the session's"
		if d.Tools != nil {
			tl = strings.Join(d.Tools, ", ")
		}
		if len(d.DisallowedTools) > 0 {
			tl += " less " + strings.Join(d.DisallowedTools, ", ")
		}
		rows = append(rows, []string{name, orDefault(d.Source, "builtin"), model, tl, oneLine(d.Description, 60)})
	}
	body := []ui.Block{{Kind: ui.BlockTable, Rows: rows}}
	ws := st.appCfg.Workspace
	if len(ws.Agents) > 0 {
		state := "trusted"
		if !ws.AgentsTrusted {
			state = "not trusted (" + orDefault(ws.AgentsReason, "not asked") + "), so not offered; `abhed trust` reviews them"
		}
		body = append(body, ui.Block{Kind: ui.BlockNotice, Text: fmt.Sprintf("workspace definitions in %s: %s",
			config.WorkspaceAgentsDir, state)})
	}
	body = append(body, ui.Block{Kind: ui.BlockNotice, Text: "definitions are read from /etc/abhed/agents, ~/.abhed/agents and, once trusted, .abhed/agents"})
	return false, e.ui.Panel(ctx, ui.PanelSpec{Title: "Agents", Body: body})
}

// slashSkills is /skills.
func slashSkills(ctx context.Context, e *cmdEnv, _ []string) (bool, error) {
	st := e.st
	if st.set == nil || st.set.Skills == nil || st.set.Skills.Len() == 0 {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "no skills are loaded; skills.dirs in the configuration names where they are read from"})
		return false, nil
	}
	rows := [][]string{{"skill", "from", "use"}}
	for _, s := range st.set.Skills.All() {
		rows = append(rows, []string{s.Name, config.Printable(s.Dir), oneLine(s.Description, 70)})
	}
	return false, e.ui.Panel(ctx, ui.PanelSpec{Title: "Skills", Body: []ui.Block{{Kind: ui.BlockTable, Rows: rows}}})
}

// slashMCP is /mcp: every enabled server's state and tools, and restart.
func slashMCP(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
	st := e.st
	if st.set == nil || st.set.Gateway == nil {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "no MCP servers are configured"})
		return false, nil
	}
	gw := st.set.Gateway
	if len(args) > 0 {
		if args[0] != "restart" || len(args) != 2 {
			return false, fmt.Errorf("usage: /mcp, or /mcp restart <server>")
		}
		if err := gw.Restart(ctx, args[1]); err != nil {
			return false, fmt.Errorf("mcp server %s did not reconnect: %w", args[1], err)
		}
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "reconnected " + args[1] + "; its tools reach the new connection, and tools it added are offered from the next session"})
		return false, nil
	}
	servers := gw.Servers()
	if len(servers) == 0 {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "no MCP servers are enabled"})
		return false, nil
	}
	rows := [][]string{{"server", "transport", "state", "tools"}}
	for _, s := range servers {
		state := "connected"
		if !s.Connected {
			state = "not connected"
			if s.Err != nil {
				state += ": " + oneLine(s.Err.Error(), 60)
			}
		}
		rows = append(rows, []string{s.Name, s.Transport, state, oneLine(strings.Join(s.Tools, ", "), 70)})
	}
	body := []ui.Block{{Kind: ui.BlockTable, Rows: rows},
		{Kind: ui.BlockNotice, Text: "every MCP call goes through policy and asks unless a rule allows it; its output is treated as untrusted"}}
	if st.registry != nil {
		if _, ok := st.registry.Get("tool_search"); ok {
			body = append(body, ui.Block{Kind: ui.BlockNotice, Text: fmt.Sprintf("more than %d MCP tools: they are offered through tool_search and loaded when asked for", toolset.DeferThreshold)})
		}
	}
	return false, e.ui.Panel(ctx, ui.PanelSpec{Title: "MCP", Body: body})
}

// slashTools is /tools: every tool, where it came from, and whether it changes things.
func slashTools(ctx context.Context, e *cmdEnv, _ []string) (bool, error) {
	st := e.st
	reg := st.registry
	if st.loop != nil {
		reg = st.loop.Tools
	}
	if reg == nil {
		return false, fmt.Errorf("no tools in this session")
	}
	rows := [][]string{{"tool", "from", "changes things"}}
	shown, deferred := 0, 0
	for _, t := range reg.All() {
		if h, ok := t.(tools.Hidden); ok && h.Hidden() {
			deferred++
			continue
		}
		from := "built in"
		switch n := t.Name(); {
		case strings.HasPrefix(n, "mcp__"):
			from = "MCP " + strings.SplitN(strings.TrimPrefix(n, "mcp__"), "__", 2)[0]
		case st.set != nil && st.set.Extensions != nil && isExtensionTool(st.set, n):
			from = "extension"
		}
		rows = append(rows, []string{t.Name(), from, yesNo(t.Mutates())})
		shown++
	}
	body := []ui.Block{{Kind: ui.BlockTable, Rows: rows}}
	if deferred > 0 {
		body = append(body, ui.Block{Kind: ui.BlockNotice, Text: strconv.Itoa(deferred) + " MCP tools more, offered through tool_search"})
	}
	return false, e.ui.Panel(ctx, ui.PanelSpec{Title: fmt.Sprintf("Tools (%d)", shown), Body: body})
}

// isExtensionTool reports whether an extension provides the tool.
func isExtensionTool(set *toolset.Set, name string) bool {
	for _, n := range set.ExtensionToolNames() {
		if n == name {
			return true
		}
	}
	return false
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// oneLine is s on one line, cut to n characters.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
