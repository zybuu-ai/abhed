package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// slashUsage is /usage (and /cost): the session's tokens, how much of the
// prompt the cache served, and where the tokens went.
func slashUsage(_ context.Context, e *cmdEnv, _ []string) (bool, error) {
	st := e.st
	u := st.total
	if u.InputTokens == 0 {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "no usage yet this session"})
		return false, nil
	}
	rows := [][]string{{"", ""},
		{"turns", strconv.Itoa(u.Turns)},
		{"tokens in", strconv.Itoa(u.InputTokens)},
		{"tokens out", strconv.Itoa(u.OutputTokens)},
		{"cached", fmt.Sprintf("%d (%.0f%%)", u.CachedTokens, u.CacheHitRate()*100)},
	}
	if savings := u.PrefillSavings(); savings > 0 {
		rows = append(rows, []string{"prefill saving", fmt.Sprintf("%.1fx", savings)})
	}
	rows = append(rows, []string{"compactions", strconv.Itoa(u.Compactions)})
	blocks := []ui.Block{{Kind: ui.BlockTable, Rows: rows}}
	if events, err := st.store.Events(st.sessionID); err == nil {
		if b := usageBreakdown(events); len(b) > 1 {
			blocks = append(blocks, ui.Block{Kind: ui.BlockTable, Rows: b})
		}
	}
	return false, e.ui.Panel(context.Background(), ui.PanelSpec{Title: "Usage", Body: blocks})
}

// usageBreakdown is tokens by subagent, and tool calls by where the tool
// came from, from the record.
func usageBreakdown(events []agent.Event) [][]string {
	rows := [][]string{{"by", "tokens in", "tokens out", "calls"}}
	type agg struct{ in, out, calls int }
	subs := map[string]*agg{}
	tools := map[string]*agg{}
	for _, ev := range events {
		switch ev.Type {
		case agent.EvSubagentReturn:
			var p struct {
				Description string `json:"description"`
				TokensIn    int    `json:"tokens_in"`
				TokensOut   int    `json:"tokens_out"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil {
				k := "subagent: " + orDefault(p.Description, "(unnamed)")
				a := subs[k]
				if a == nil {
					a = &agg{}
					subs[k] = a
				}
				a.in, a.out, a.calls = a.in+p.TokensIn, a.out+p.TokensOut, a.calls+1
			}
		case agent.EvActionRequested:
			var p struct {
				Tool string `json:"tool"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil && p.Tool != "" {
				k := "tools: built in"
				if strings.HasPrefix(p.Tool, "mcp__") {
					k = "tools: MCP " + strings.SplitN(strings.TrimPrefix(p.Tool, "mcp__"), "__", 2)[0]
				}
				a := tools[k]
				if a == nil {
					a = &agg{}
					tools[k] = a
				}
				a.calls++
			}
		}
	}
	for _, m := range []map[string]*agg{subs, tools} {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			a := m[k]
			in, out := "", ""
			if strings.HasPrefix(k, "subagent") {
				in, out = strconv.Itoa(a.in), strconv.Itoa(a.out)
			}
			rows = append(rows, []string{k, in, out, strconv.Itoa(a.calls)})
		}
	}
	return rows
}

// statusModel is what the footer, /status and a statusline command show.
func (c *cliState) statusModel(mode string) ui.StatusModel {
	m := ui.StatusModel{
		Provider:   c.appCfg.Model.Default,
		Mode:       mode,
		ModeLocked: c.appCfg.ManagedSets("permissions.mode"),
		TokensIn:   c.total.InputTokens,
		TokensOut:  c.total.OutputTokens,
		Record:     ui.RecordMemory,
		GitBranch:  gitBranch(c.workspace),
		Network:    c.appCfg.Sandbox.AllowNetwork,
	}
	if c.adapter != nil {
		m.Model = c.adapter.Profile().Name
	}
	if c.appCfg.Storage.Driver == "postgres" {
		m.Record = ui.RecordUnverified
	}
	if c.sandbox != nil {
		if c.sandbox.Resolved() {
			m.SandboxTier = string(c.sandbox.Tier())
		} else {
			m.SandboxTier = string(c.sandbox.floor)
		}
		if m.SandboxTier == string(sandbox.TierNone) {
			m.Network = true
		}
	}
	m.BackgroundTasks = c.liveTasks()
	if c.store != nil && c.sessionID != "" {
		if events, err := c.store.Events(c.sessionID); err == nil {
			m.ContextTokens, m.ContextPercent = contextUse(events)
		}
	}
	return m
}

// contextUse is how much of the window the last model call's prompt took.
func contextUse(events []agent.Event) (int, int) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != agent.EvModelCall {
			continue
		}
		var p agent.ModelCall
		if json.Unmarshal(events[i].Payload, &p) != nil {
			return 0, 0
		}
		if p.ContextWindow > 0 {
			return p.TokensIn, min(100, p.TokensIn*100/p.ContextWindow)
		}
		return p.TokensIn, 0
	}
	return 0, 0
}

// gitBranch reads the workspace's branch from .git/HEAD, without running git.
func gitBranch(ws string) string {
	data, err := os.ReadFile(filepath.Join(ws, ".git", "HEAD")) // #nosec G304 -- the workspace's .git/HEAD, read for the branch name
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(data))
	if b, ok := strings.CutPrefix(ref, "ref: refs/heads/"); ok {
		return b
	}
	if len(ref) >= 7 {
		return ref[:7]
	}
	return ""
}

// slashStatus is /status.
func slashStatus(ctx context.Context, e *cmdEnv, _ []string) (bool, error) {
	st := e.st
	m := st.statusModel(string(e.pol.Mode))
	cfg := st.appCfg
	mode := m.Mode
	if m.ModeLocked {
		mode += " (set by the managed configuration)"
	}
	sb := orDefault(m.SandboxTier, "unknown")
	if st.sandbox != nil && !st.sandbox.Resolved() {
		sb += " or stronger (still checking)"
	}
	if m.Network {
		sb += " · network on"
	} else {
		sb += " · no network"
	}
	record := string(m.Record)
	switch m.Record {
	case ui.RecordMemory:
		record = "memory only: this session is gone when Abhed exits"
	case ui.RecordUnverified:
		record = storageLabel(cfg)
	}
	session := orDefault(st.sessionID, "not started")
	turns := "no limit"
	if cfg.Limits.MaxTurns > 0 {
		turns = fmt.Sprintf("%d model turns across the whole conversation; /clear starts a new one", cfg.Limits.MaxTurns)
	}
	budget := "none"
	if cfg.Limits.MaxBudgetTokens > 0 {
		budget = fmt.Sprintf("%d tokens, shared with subagents", cfg.Limits.MaxBudgetTokens)
	}
	rows := [][]string{{"", ""},
		{"model", fmt.Sprintf("%s (%s)", m.Model, m.Provider)},
		{"mode", mode},
		{"workspace", st.workspace},
		{"branch", orDefault(m.GitBranch, "-")},
		{"sandbox", sb},
		{"record", record},
		{"session", session},
		{"context", fmt.Sprintf("%d tokens (%d%% of the window)", m.ContextTokens, m.ContextPercent)},
		{"turn limit", turns},
		{"token budget", budget},
		{"background", fmt.Sprintf("%d task(s) running", m.BackgroundTasks)},
		{"trust", trustLine(cfg.Workspace)},
	}
	if cfg.Managed {
		rows = append(rows, []string{"managed", managed.ConfigFile + ": " + orDefault(strings.Join(cfg.ManagedKeys, ", "), "no settings")})
	}
	if line := st.statusLine(ctx, string(e.pol.Mode)); line != "" {
		rows = append(rows, []string{"statusline", line})
	}
	for _, k := range cfg.NotYetInEffect() {
		rows = append(rows, []string{"not in effect", config.NotYetInEffectMessage(k)})
	}
	return false, e.ui.Panel(ctx, ui.PanelSpec{Title: "Status", Body: []ui.Block{{Kind: ui.BlockTable, Rows: rows}}})
}

// trustLine says what was decided about the workspace's own configuration.
func trustLine(st config.WorkspaceTrust) string {
	switch {
	case st.File == "" && len(st.Agents) == 0:
		return "the workspace has no configuration of its own"
	case st.Trusted:
		return "the workspace configuration is trusted (" + st.Reason + ")"
	}
	return "the workspace configuration is not trusted (" + orDefault(st.Reason, "not asked") + "); only its tightening settings apply"
}
