package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
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

// A managed rule that can never match fails the doctor; a managed deny rule
// stops the configuration loading at all.
func TestDoctorFailsOnAManagedRuleThatNeverMatches(t *testing.T) {
	managedConfig(t, `{"permissions":{"ask":["Shell(git push*)"]}}`)
	out, code, js, jsonCode := doctorWithRules(t, "")
	if code == 0 || !strings.Contains(out, "managed configuration has a permission rule that can never match") {
		t.Fatalf("managed ask (%d):\n%s", code, out)
	}
	if jsonCode == 0 || !strings.Contains(js, `"status": "fail"`) {
		t.Fatalf("managed ask, JSON (%d):\n%s", jsonCode, js)
	}

	managedConfig(t, `{"permissions":{"deny":["Shell(curl *)"]}}`)
	_, code, js, jsonCode = doctorWithRules(t, "")
	if code == 0 || jsonCode == 0 || !strings.Contains(js, "Shell(curl *) can never match") {
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
		t.Errorf("a managed rule that never matches passed:\n%s", b.String())
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
