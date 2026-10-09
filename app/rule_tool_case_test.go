package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// doctorWithRules runs the doctor and its JSON on a workspace whose configuration
// adds permissions to a working model.
func doctorWithRules(t *testing.T, permissions string) (text string, code int, jsonOut string, jsonCode int) {
	t.Helper()
	srv := doctorEndpoint(t)
	ws := t.TempDir()
	cfg := `{"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + srv.URL + `","model":"m","context_window":8192}}}` + permissions + `}`
	_ = os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755)
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.TrustEnv, "1")
	text, code = stdoutOf(t, func() int { return newApp().doctor(ws) })
	var b strings.Builder
	jsonCode = newApp().doctorJSON(&b, ws)
	return text, code, b.String(), jsonCode
}

// A rule naming no tool is reported by the doctor and its JSON, with what was meant.
func TestDoctorReportsARuleNamingNoTool(t *testing.T) {
	managedConfig(t, "")
	out, _, js, _ := doctorWithRules(t, `,"permissions":{"deny":["WebFetch(https://example.com/*)"]}`)
	want := `no built-in tool is named \"WebFetch\" (did you mean web_fetch?)`
	if !strings.Contains(out, strings.ReplaceAll(want, `\"`, `"`)) {
		t.Fatalf("the doctor does not report the rule:\n%s", out)
	}
	if !strings.Contains(js, `"id": "rules"`) || !strings.Contains(js, want) {
		t.Fatalf("doctor --json does not report the rule:\n%s", js)
	}
}

// A managed ask rule naming no tool fails the doctor; a managed deny rule
// that evidently misnames its tool stops the configuration loading at all.
func TestDoctorFailsOnAManagedRuleThatNeverMatches(t *testing.T) {
	managedConfig(t, `{"permissions":{"ask":["Shell(git push*)"]}}`)
	out, code, js, jsonCode := doctorWithRules(t, "")
	if code == 0 || !strings.Contains(out, "managed configuration has a deny or ask rule that names no tool as written") {
		t.Fatalf("managed ask (%d):\n%s", code, out)
	}
	if jsonCode == 0 || !strings.Contains(js, `"status": "fail"`) {
		t.Fatalf("managed ask, JSON (%d):\n%s", jsonCode, js)
	}

	managedConfig(t, `{"permissions":{"deny":["Shell(curl *)"]}}`)
	_, code, js, jsonCode = doctorWithRules(t, "")
	if code == 0 || jsonCode == 0 || !strings.Contains(js, "Shell(curl *) names its tool wrongly") {
		t.Fatalf("managed deny (%d, %d):\n%s", code, jsonCode, js)
	}
}

func TestDoctorVerdictOnRuleFindings(t *testing.T) {
	var b strings.Builder
	if code := doctorVerdict(&b, configCheck{rules: true}); code != 0 {
		t.Errorf("a person's own rule warning failed the doctor:\n%s", b.String())
	}
	b.Reset()
	if code := doctorVerdict(&b, configCheck{rules: true, neverManaged: true}); code != 1 {
		t.Errorf("a managed deny or ask rule naming no tool passed:\n%s", b.String())
	}
}

// The familiar flags fold only Abhed's own names: an MCP tool keeps its case.
func TestToolRuleKeepsMCPNames(t *testing.T) {
	for in, want := range map[string]string{
		"mcp__GitHub__create_issue": "mcp__GitHub__create_issue",
		"BASH(curl:*)":              "bash(curl*)",
		"WebFetch":                  "web_fetch",
		"Read":                      "read",
	} {
		if got := toolRule(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// -disallowedTools lowered every tool name up to 1.2.7, so a deny for an MCP
// tool keeps matching the lowered spelling as well as its own; an allow from
// -allowedTools stays exact.
func TestDisallowedToolsKeepsTheLoweredSpelling(t *testing.T) {
	cfg, err := applyFlags(config.Default(), "", 0,
		joinRules("", "mcp__GitHub__list"), joinDenyRules("", "mcp__GitHub__create_issue(*),Bash(curl:*)"), "")
	if err != nil {
		t.Fatal(err)
	}
	pol := policy.New(policy.ModeBypass)
	if err := pol.AddDeny(cfg.Permissions.Deny...); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"mcp__github__create_issue", "mcp__GitHub__create_issue"} {
		if d := pol.Evaluate(tool, true, json.RawMessage(`{}`)); d.Decision != policy.Deny {
			t.Errorf("%s: %s, want deny (rules %q)", tool, d.Decision, cfg.Permissions.Deny)
		}
	}
	if slices.Contains(cfg.Permissions.Allow, "mcp__github__list") || !slices.Contains(cfg.Permissions.Allow, "mcp__GitHub__list") {
		t.Errorf("allow rules %q", cfg.Permissions.Allow)
	}
	if n := len(cfg.Permissions.Deny) - len(config.Default().Permissions.Deny); n != 3 {
		t.Errorf("deny rules %q", cfg.Permissions.Deny)
	}
}

func TestDoctorFailsOnOverlappingMCPServers(t *testing.T) {
	var b strings.Builder
	if code := doctorVerdict(&b, configCheck{mcpOverlap: true}); code != 1 || !strings.Contains(b.String(), "MCP servers' names overlap") {
		t.Fatalf("%d:\n%s", code, b.String())
	}
	cfg := config.Default()
	cfg.MCP.Servers = []config.MCPServerConfig{{Name: "a", Command: "true", Enabled: true}, {Name: "a__b", Command: "true", Enabled: true}}
	b.Reset()
	if f := configFindings(&b, cfg); !f.mcpOverlap {
		t.Fatalf("not found:\n%s", b.String())
	}
}
