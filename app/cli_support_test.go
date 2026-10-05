package app

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	root "github.com/zybuu-ai/abhed"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/ui"
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

// A wait for drawing returns at once when drawing has caught up, follows it
// while it moves, and gives up soon after it stops: a stalled backlog cost
// every queued command a whole second.
func TestWaitRenderedFollowsDrawing(t *testing.T) {
	var c cliState
	c.rendered.Store(5)
	start := time.Now()
	c.waitRendered(5)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("waited %v for drawing already done", d)
	}
	start = time.Now()
	c.waitRendered(9) // nothing draws
	if d := time.Since(start); d > renderWait/2 {
		t.Fatalf("waited %v on drawing that had stopped", d)
	}
	go func() {
		for i := int64(6); i <= 9; i++ {
			time.Sleep(20 * time.Millisecond)
			c.rendered.Store(i)
		}
	}()
	c.waitRendered(9)
	if got := c.rendered.Load(); got != 9 {
		t.Fatalf("returned at %d while drawing still moved", got)
	}
}

// /fork's list names a pipeline's step as the pipeline's, since forking there
// forks before the skill call that ran it.
func TestForkPointsNameAPipelineStep(t *testing.T) {
	step, _ := json.Marshal(agent.ActionRequested{CallID: "step_1", Tool: "bash", Args: json.RawMessage(`{"command":"ls"}`), Via: "skill research pipeline"})
	out, _ := stdoutOf(t, func() int {
		forkPoints(ui.NewRenderer(io.Discard, false), []agent.Event{{Seq: 3, Type: agent.EvActionRequested, Payload: step}})
		return 0
	})
	if !strings.Contains(out, `bash {"command":"ls"} (skill research pipeline)`) {
		t.Fatalf("listed:\n%s", out)
	}
}
