package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

func workspaceWith(t *testing.T, body string) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

func engineFor(t *testing.T, cfg Config) *policy.Engine {
	t.Helper()
	e := policy.New(policy.ModeBypass)
	if err := e.AddDeny(cfg.Permissions.Deny...); err != nil {
		t.Fatal(err)
	}
	return e
}

func curlCall() json.RawMessage { return json.RawMessage(`{"command":"curl https://example.com"}`) }

// A managed or workspace deny rule copied in Bash(...) style loads without a
// finding and denies the command.
func TestCopiedStyleDenyRulesLoadAndHold(t *testing.T) {
	withManaged(t, `{"permissions": {"deny": ["Bash(curl *)", "READ(**/.env)"]}}`)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if f := cfg.RuleFindings(); len(f) != 0 {
		t.Errorf("findings for built-in names in another case: %v", f)
	}
	if d := engineFor(t, cfg).Evaluate("bash", true, curlCall()); d.Decision != policy.Deny {
		t.Errorf("managed Bash(curl *): %s", d.Decision)
	}

	ws := workspaceWith(t, `{"permissions": {"deny": ["BASH(curl *)"]}}`)
	cfg, err = LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if d := engineFor(t, cfg).Evaluate("bash", true, curlCall()); d.Decision != policy.Deny {
		t.Errorf("workspace BASH(curl *): %s", d.Decision)
	}
	// An untrusted workspace's deny rules tighten, and fold the same way.
	cfg, err = LoadWith(ws, LoadOptions{Trust: TrustRefused, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if d := engineFor(t, cfg).Evaluate("bash", true, curlCall()); d.Decision != policy.Deny {
		t.Errorf("untrusted workspace BASH(curl *): %s", d.Decision)
	}
}

// A managed deny rule naming no tool refuses to start, naming the rule.
func TestManagedDenyNamingNoToolRefusesToStart(t *testing.T) {
	path := withManaged(t, `{"permissions": {"deny": ["Shell(curl *)"]}}`)
	_, err := Load(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "Shell(curl *)") || !strings.Contains(err.Error(), path) ||
		!strings.Contains(err.Error(), "did you mean bash?") {
		t.Fatalf("Load: %v", err)
	}
	if _, err := LoadManaged(); err == nil || !strings.Contains(err.Error(), "Shell(curl *)") {
		t.Fatalf("LoadManaged: %v", err)
	}
}

// A managed deny rule for an MCP server written in another case can never
// match, since MCP names are compared exactly; one for a server not set up
// here is a finding but not a refusal, as an organisation may deny ahead.
func TestManagedMCPDenyRules(t *testing.T) {
	withManaged(t, `{"mcp": {"servers": [{"name": "github", "command": "true"}]},
	  "permissions": {"deny": ["mcp__GitHub__create_issue"]}}`)
	if _, err := Load(t.TempDir()); err == nil || !strings.Contains(err.Error(), `configured as "github"`) {
		t.Fatalf("Load: %v", err)
	}

	withManaged(t, `{"mcp": {"servers": [{"name": "github", "command": "true"}]},
	  "permissions": {"deny": ["mcp__github__create_issue", "mcp__jira__delete"]}}`)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := cfg.RuleFindings()
	if len(f) != 1 || f[0].Rule != "mcp__jira__delete" || f[0].Never || !f[0].Managed() {
		t.Fatalf("findings: %+v", f)
	}
}

// A workspace rule naming no tool is warned of and reported, and still loads.
func TestUnknownToolRuleIsWarnedAndReported(t *testing.T) {
	var warned bytes.Buffer
	warnOut = &warned
	t.Cleanup(func() { warnOut = os.Stderr })
	t.Setenv("HOME", t.TempDir())
	ws := workspaceWith(t, `{"permissions": {"deny": ["WebFetch(domain:example.com)"], "allow": ["Bash(npm test:*)"]}}`)
	cfg, err := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	f := cfg.RuleFindings()
	if len(f) != 2 || !f[0].Never || f[0].Managed() || !strings.Contains(f[0].Why, "web_fetch") ||
		f[1].Never || !strings.Contains(f[1].Why, "bash(npm test *)") {
		t.Fatalf("findings: %+v", f)
	}
	if !strings.Contains(warned.String(), "WebFetch(domain:example.com)") {
		t.Errorf("no warning: %q", warned.String())
	}
}

// With an extension that may provide tools, an unknown name may be its tool.
func TestUnknownToolRuleWithExtensions(t *testing.T) {
	cfg := Default()
	cfg.Permissions.Deny = []string{"jira_create"}
	if f := cfg.RuleFindings(); len(f) != 1 || !f[0].Never {
		t.Fatalf("without extensions: %+v", f)
	}
	cfg.Extensions = []ExtensionConfig{{Name: "jira", Command: "true"}}
	if f := cfg.RuleFindings(); len(f) != 1 || f[0].Never {
		t.Fatalf("with an extension: %+v", f)
	}
}

// MCP rule shapes that can never match are named as such; a server wildcard
// for a configured server is no finding.
func TestMCPRuleShapes(t *testing.T) {
	cfg := Default()
	cfg.MCP.Servers = []MCPServerConfig{{Name: "github", Command: "true"}, {Name: "github__ent", Command: "true"}}
	for rule, want := range map[string]string{
		"mcp__github__*":      "also covers every tool of the MCP server \"github__ent\"",
		"mcp__github__x (*)":  "",
		"mcp__github__create": "",
		"mcp__github":         "names the MCP server \"github\" but no tool",
		"mcp__github__x*":     "the only wildcard",
		"mcp__GitHub__*":      "configured as \"github\"",
		"mcp__gitlab__create": "no MCP server \"gitlab\"",
		"mcp__github__ent__*": "",
	} {
		cfg.Permissions.Deny = []string{rule}
		f := cfg.RuleFindings()
		switch {
		case want == "" && len(f) != 0:
			t.Errorf("%s: %v", rule, f)
		case want != "" && (len(f) != 1 || !strings.Contains(f[0].Why, want)):
			t.Errorf("%s: %+v, want %q", rule, f, want)
		}
	}
}
