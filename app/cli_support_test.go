package app

import (
	"strings"
	"testing"

	root "github.com/zybuu-ai/abhed"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/secrets"
)

func TestReleaseNotes(t *testing.T) {
	log := "# Changelog\n\n## [Unreleased]\n\n- new\n\n## [1.2.1] - 2026-09-28\n\n- fix\n\n## [1.2.0]\n\n- old\n"
	for v, want := range map[string]string{"dev": "- new", "": "- new", "v1.2.1": "- fix", "1.2.0": "- old"} {
		got, ok := releaseNotes(log, v)
		if !ok || !strings.Contains(got, want) || strings.Count(got, "## [") != 1 {
			t.Errorf("%q: %v %q", v, ok, got)
		}
	}
	if all, _ := releaseNotes(log, "all"); strings.Count(all, "## [") != 3 {
		t.Errorf("all: %q", all)
	}
	if _, ok := releaseNotes(log, "9.9.9"); ok {
		t.Error("a missing version was found")
	}
	if _, ok := releaseNotes(root.Changelog, "dev"); !ok {
		t.Error("the embedded changelog has no Unreleased section")
	}
}

// The report names no endpoint and no home directory.
func TestBugReportRedacts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	st := &cliState{appCfg: config.Default(), workspace: home + "/ws", version: "1.2.3",
		provider: config.ProviderConfig{Type: "openai-compatible", BaseURL: "http://10.1.2.3:8000/v1"}}
	title, body := bugReport(st, "it broke in "+home+"/ws")
	if strings.Contains(title+body, home) || strings.Contains(body, "10.1.2.3") || !strings.Contains(body, "abhed 1.2.3") ||
		!strings.Contains(title, "~/ws") {
		t.Fatalf("%q\n%s", title, body)
	}
}

// A value in the vault and the provider's key from its variable never reach
// the report, in plain text as typed.
func TestBugReportRedactsSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(secrets.EnvFile, "")
	if err := openVault().Set("TOKEN", "vault-canary-value-1"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ABHED_TEST_BUG_KEY", "provider-canary-key-9")
	st := &cliState{appCfg: config.Default(), version: "1.2.3",
		provider: config.ProviderConfig{Type: "openai-compatible", APIKeyEnv: "ABHED_TEST_BUG_KEY"}}
	title, body := bugReport(st, "it printed vault-canary-value-1 and provider-canary-key-9")
	for _, v := range []string{"vault-canary-value-1", "provider-canary-key-9"} {
		if strings.Contains(title+body, v) {
			t.Fatalf("%s leaked:\n%s\n%s", v, title, body)
		}
	}
	if !strings.Contains(body, "it printed") {
		t.Fatalf("the report was withheld whole:\n%s", body)
	}
}

// The provider's key held in the configuration itself is redacted too.
func TestBugReportRedactsTheProviderKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(secrets.EnvFile, "")
	st := &cliState{appCfg: config.Default(), provider: config.ProviderConfig{Type: "openai-compatible", APIKey: "inline-canary-key-7"}}
	title, body := bugReport(st, "saw inline-canary-key-7")
	if strings.Contains(title+body, "inline-canary-key-7") || !strings.Contains(body, "[redacted key]") {
		t.Fatalf("%s\n%s", title, body)
	}
}
