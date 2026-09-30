package app

import (
	"strings"
	"testing"

	root "github.com/zybuu-ai/abhed"
	"github.com/zybuu-ai/abhed/config"
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
