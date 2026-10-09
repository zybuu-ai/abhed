package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/rag"
)

// RuleFinding is a permission rule whose tool name names no tool this
// configuration has, or one only in another case, or whose pattern is
// written in another tool's syntax.
type RuleFinding struct {
	List  string `json:"list"` // allow, ask or deny
	Rule  string `json:"rule"`
	Layer string `json:"layer,omitempty"`
	// Never is set when no call in this configuration can match the rule.
	Never bool   `json:"never"`
	Why   string `json:"why"`
}

// Managed reports whether the organisation's file set the rule.
func (f RuleFinding) Managed() bool { return f.Layer == LayerManaged }

func (f RuleFinding) String() string {
	s := fmt.Sprintf("permissions.%s rule %s: %s", f.List, Printable(f.Rule), f.Why)
	if f.Managed() {
		s += "; it is in the managed configuration, which whoever manages it must correct"
	}
	return s
}

// RuleFindings lists the permission rules that name no tool as written, or
// use another tool's pattern syntax. A built-in name in any case is not one:
// it is read as Abhed spells it.
func (c Config) RuleFindings() []RuleFinding {
	var out []RuleFinding
	for _, l := range []struct {
		name  string
		rules []string
	}{{"deny", c.Permissions.Deny}, {"ask", c.Permissions.Ask}, {"allow", c.Permissions.Allow}} {
		for _, raw := range l.rules {
			r, err := policy.ParseRule(raw)
			if err != nil {
				continue // Validate reports it
			}
			never, why := c.ruleToolProblem(r.Tool())
			if why == "" {
				why = prefixSyntax(r)
			}
			if why != "" {
				out = append(out, RuleFinding{List: l.name, Rule: strings.TrimSpace(raw),
					Layer: c.RuleLayer(l.name, raw), Never: never, Why: why})
			}
		}
	}
	return out
}

// ruleToolProblem says why a rule's tool name may match nothing, and whether
// it certainly matches nothing here; "" when it names a tool.
func (c Config) ruleToolProblem(tool string) (never bool, why string) {
	if tool == "*" || policy.BuiltinTool(tool) {
		return false, ""
	}
	lower := strings.ToLower(tool)
	if strings.HasPrefix(lower, "mcp__") {
		return c.mcpProblem(tool)
	}
	for _, corpus := range c.RAG.Corpora {
		name := rag.ToolName(corpus.Name)
		if tool == name {
			return false, ""
		}
		if strings.EqualFold(tool, name) {
			return true, fmt.Sprintf("the knowledge-base tool is %s, and a tool name other than a built-in one is compared exactly, so the rule never matches", name)
		}
	}
	ext := c.extensionsMayProvideTools()
	why = fmt.Sprintf("no built-in tool is named %q", tool)
	if alt := policy.SuggestTool(tool); alt != "" {
		why += fmt.Sprintf(" (did you mean %s?)", alt)
	}
	if ext {
		return false, why + "; it matches only a tool an extension provides under exactly that name"
	}
	return true, why + ", and no extension provides tools, so the rule never matches"
}

// mcpProblem checks an MCP tool name, mcp__server__tool or mcp__server__*,
// against the configured servers. Server and tool names are compared
// exactly, as MCP does.
func (c Config) mcpProblem(tool string) (never bool, why string) {
	wild, wildcard := policy.MCPServerWildcard(tool)
	if !wildcard && strings.ContainsAny(tool, "*? \t") {
		return true, "the only wildcard in an MCP tool name is a whole tool part, mcp__server__*, so the rule never matches"
	}
	if wildcard {
		configured := false
		for _, s := range c.MCP.Servers {
			configured = configured || "mcp__"+s.Name+"__" == wild
		}
		for _, s := range c.MCP.Servers {
			if other := "mcp__" + s.Name + "__"; configured && other != wild && strings.HasPrefix(other, wild) {
				return false, fmt.Sprintf("it also covers every tool of the MCP server %q, whose tools are named %s…", s.Name, other)
			}
		}
		if configured {
			return false, ""
		}
	}
	for _, s := range c.MCP.Servers {
		prefix := "mcp__" + s.Name + "__"
		switch {
		case tool == "mcp__"+s.Name || tool == prefix:
			return true, fmt.Sprintf("it names the MCP server %q but no tool of it; write %s<tool> for one tool, or %s* for all of them", s.Name, prefix, prefix)
		case !wildcard && strings.HasPrefix(tool, prefix):
			return false, ""
		}
	}
	lower := strings.ToLower(tool)
	for _, s := range c.MCP.Servers {
		if prefix := strings.ToLower("mcp__" + s.Name + "__"); strings.HasPrefix(lower, prefix) || lower == strings.TrimSuffix(prefix, "__") {
			return true, fmt.Sprintf("the MCP server is configured as %q, and MCP tool names are compared exactly, case included, so the rule never matches", s.Name)
		}
	}
	if !strings.HasPrefix(tool, "mcp__") {
		return !c.extensionsMayProvideTools(), "MCP tools are named mcp__server__tool in lower-case mcp__, so the rule names no MCP tool"
	}
	server, name, _ := strings.Cut(strings.TrimPrefix(tool, "mcp__"), "__")
	if name == "" {
		return true, "it names no tool: write mcp__<server>__<tool>, or mcp__<server>__* for every tool of a server"
	}
	return false, fmt.Sprintf("no MCP server %q is configured here, so the rule matches nothing until one is", server)
}

// extensionsMayProvideTools reports whether a configured extension may offer
// tools, whose names are known only once it runs.
func (c Config) extensionsMayProvideTools() bool {
	for _, e := range c.ExtensionSpecs() {
		if len(e.Events) == 0 || slices.Contains(e.Events, extension.EvListTools) {
			return true
		}
	}
	return false
}

// prefixSyntax notes a bash pattern ending in :*, another tool's way of
// writing a prefix, which Abhed reads literally.
func prefixSyntax(r policy.Rule) string {
	if r.Tool() != "bash" {
		return ""
	}
	_, glob, ok := strings.Cut(r.Canonical(), "(")
	if !ok || !strings.HasSuffix(glob, ":*)") {
		return ""
	}
	return fmt.Sprintf("the pattern ends in :*, which is read literally (a colon, then anything); write bash(%s *) for a command and its arguments",
		strings.TrimSuffix(glob, ":*)"))
}

// managedNeverDenies is the error for a managed deny rule that can never
// match: Abhed does not run believing a rule holds that cannot.
func managedNeverDenies(findings []RuleFinding) error {
	for _, f := range findings {
		if f.Managed() && f.List == "deny" && f.Never {
			return fmt.Errorf("%s: permissions.deny rule %s can never match: %s. Abhed will not start with an organisation's deny rule that cannot hold; "+
				"correct the rule in the managed configuration (built-in tool names: %s)",
				Printable(managed.ConfigFile), Printable(f.Rule), f.Why, strings.Join(policy.BuiltinTools(), ", "))
		}
	}
	return nil
}
