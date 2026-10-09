package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/mcp"
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
	Never bool `json:"never"`
	// Mistake is set when the name is evidently another's: another harness's
	// name for a built-in, or a built-in, managed server or corpus in another
	// case or in look-alike letters. A managed deny rule so written stops startup.
	Mistake bool   `json:"mistake,omitempty"`
	Why     string `json:"why"`
}

// FailsDoctor reports whether the finding makes the doctor fail: a managed
// deny or ask rule, which the organisation relies on to hold. A managed allow
// rule that matches nothing only allows less, so it is a warning.
func (f RuleFinding) FailsDoctor() bool { return f.Managed() && f.List != "allow" }

// problem is why a rule's tool name may match nothing.
type problem struct {
	never, mistake bool
	why            string
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
			p := c.ruleToolProblem(r.Tool(), l.name)
			if p.why == "" {
				p.why = prefixSyntax(r)
			}
			if p.why != "" {
				out = append(out, RuleFinding{List: l.name, Rule: strings.TrimSpace(raw),
					Layer: c.RuleLayer(l.name, raw), Never: p.never, Mistake: p.mistake, Why: p.why})
			}
		}
	}
	return out
}

// ruleToolProblem says why a rule's tool name may match nothing; a zero
// problem when it names a tool.
func (c Config) ruleToolProblem(tool, list string) problem {
	if tool == "*" || policy.BuiltinTool(tool) {
		return problem{}
	}
	if strings.HasPrefix(strings.ToLower(tool), "mcp__") {
		return c.mcpProblem(tool, list)
	}
	for _, corpus := range c.RAG.Corpora {
		name := rag.ToolName(corpus.Name)
		if tool == name {
			return problem{}
		}
		if strings.EqualFold(tool, name) {
			return problem{never: true, mistake: c.ManagedSets("rag"),
				why: fmt.Sprintf("the knowledge-base tool is %s, and a tool name other than a built-in one is compared exactly, so the rule never matches", name)}
		}
	}
	ext := c.extensionsMayProvideTools()
	if strings.HasPrefix(tool, "rag_") {
		return problem{never: !ext, why: fmt.Sprintf("no knowledge base here is searched with %s, so the rule matches nothing until one is", tool)}
	}
	why := fmt.Sprintf("no built-in tool is named %q", tool)
	alt, evident := policy.SuggestTool(tool), false
	for _, n := range policy.BuiltinTools() {
		if strings.EqualFold(tool, n) {
			// Only a look-alike letter, such as the Kelvin sign for k, is left to differ.
			alt, evident = n, true
		}
	}
	if alt != "" {
		why += fmt.Sprintf(" (did you mean %s?)", alt)
		// A near miss such as git or sql may be a tool meant ahead: a finding, not a refusal.
		// Another harness's name (Shell) is evident unless an extension may offer a tool so named.
		evident = evident || policy.FamiliarName(tool) && !ext
	}
	if ext {
		return problem{mistake: evident, why: why + "; it matches only a tool an extension provides under exactly that name"}
	}
	return problem{never: true, mistake: evident, why: why + ", and no extension provides tools, so the rule never matches"}
}

// mcpProblem checks an MCP tool name, mcp__server__tool or mcp__server__*,
// against the configured servers. Server and tool names are compared
// exactly, as MCP does.
func (c Config) mcpProblem(tool, list string) problem {
	wild, wildcard := policy.MCPServerWildcard(tool)
	if !wildcard && strings.ContainsAny(tool, "*? \t") {
		return problem{never: true, why: "the only wildcard in an MCP tool name is a whole tool part, mcp__server__*, so the rule never matches"}
	}
	if wildcard {
		configured := false
		for _, s := range c.MCP.Servers {
			configured = configured || "mcp__"+s.Name+"__" == wild
		}
		// Server and tool names may hold "__", so a wildcard can reach past the
		// server it was written for; wider than meant only matters where it allows.
		for _, s := range c.MCP.Servers {
			other := "mcp__" + s.Name + "__"
			switch {
			case list != "allow" || other == wild:
			case configured && strings.HasPrefix(other, wild):
				return problem{why: fmt.Sprintf("it also covers every tool of the MCP server %q, whose tools are named %s…", s.Name, other)}
			case strings.HasPrefix(wild, other):
				return problem{why: fmt.Sprintf("it also covers the tools of the MCP server %q whose names begin %s", s.Name, strings.TrimPrefix(wild, other))}
			}
		}
		if configured {
			return problem{}
		}
	}
	for _, s := range c.MCP.Servers {
		prefix := "mcp__" + s.Name + "__"
		switch {
		case tool == "mcp__"+s.Name || tool == prefix:
			return problem{never: true, why: fmt.Sprintf("it names the MCP server %q but no tool of it; write %s<tool> for one tool, or %s* for all of them", s.Name, prefix, prefix)}
		case !wildcard && strings.HasPrefix(tool, prefix):
			return problem{}
		}
	}
	lower := strings.ToLower(tool)
	for _, s := range c.MCP.Servers {
		if prefix := strings.ToLower("mcp__" + s.Name + "__"); strings.HasPrefix(lower, prefix) || lower == strings.TrimSuffix(prefix, "__") {
			// Evident only for a server the organisation set: a person's own
			// server named in another case must not stop every machine starting.
			return problem{never: true, mistake: c.ManagedSets("mcp"),
				why: fmt.Sprintf("the MCP server is configured as %q, and MCP tool names are compared exactly, case included, so the rule never matches", s.Name)}
		}
	}
	if !strings.HasPrefix(tool, "mcp__") {
		ext := c.extensionsMayProvideTools()
		return problem{never: !ext, mistake: !ext,
			why: "MCP tools are named mcp__server__tool in lower-case mcp__, so the rule names no MCP tool"}
	}
	server, name, _ := strings.Cut(strings.TrimPrefix(tool, "mcp__"), "__")
	if name == "" {
		return problem{never: true, why: "it names no tool: write mcp__<server>__<tool>, or mcp__<server>__* for every tool of a server"}
	}
	return problem{never: true, why: fmt.Sprintf("no MCP server %q is configured here, so the rule matches nothing until one is", server)}
}

// MCPServerOverlaps names each pair of enabled MCP servers where one's name
// is the other's and "__" and more: their tools' names cannot be told apart,
// so the longer-named server is not started.
func (c Config) MCPServerOverlaps() []string {
	var enabled []string
	for _, s := range c.MCP.Servers {
		if s.Enabled {
			enabled = append(enabled, s.Name)
		}
	}
	var out []string
	for _, n := range enabled {
		if o := mcp.OverlappedBy(n, enabled); o != "" {
			out = append(out, fmt.Sprintf("MCP server %q is not started: its name and server %q's overlap, so their tools' names cannot be told apart; rename one", n, o))
		}
	}
	return out
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

// managedNeverDenies is the error for a managed deny rule that evidently
// misnames its tool: Abhed does not run believing a rule holds that cannot.
// A name that may be meant ahead, a server or corpus not set up here or an
// extension's tool, is a finding, not a refusal.
func managedNeverDenies(findings []RuleFinding) error {
	for _, f := range findings {
		if f.Managed() && f.List == "deny" && f.Mistake {
			return fmt.Errorf("%s: permissions.deny rule %s names its tool wrongly: %s. Abhed will not start with an organisation's deny rule that cannot hold; "+
				"correct the rule in the managed configuration (built-in tool names: %s)",
				Printable(managed.ConfigFile), Printable(f.Rule), f.Why, strings.Join(policy.BuiltinTools(), ", "))
		}
	}
	return nil
}
