package toolset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// agentsHome isolates the home directory, the trust store and the managed
// definitions directory.
func agentsHome(t *testing.T) (home, ws string) {
	t.Helper()
	home, ws = t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(config.TrustEnv, "")
	old := managed.AgentsDir
	managed.AgentsDir = filepath.Join(t.TempDir(), "none")
	t.Cleanup(func() { managed.AgentsDir = old })
	return home, ws
}

func writeAgentFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A workspace's definitions are not offered until the workspace is trusted
// for their content; untrusted they are named as ignored.
func TestWorkspaceAgentsNeedTrust(t *testing.T) {
	home, ws := agentsHome(t)
	writeAgentFile(t, filepath.Join(ws, ".abhed", "agents"), "reviewer.md", "---\ndescription: reviews\ntools: read\n---\nReview.\n")
	writeAgentFile(t, filepath.Join(home, ".abhed", "agents"), "mine.md", "---\ndescription: the operator's\n---\nMine.\n")

	cfg, err := config.LoadWith(ws, config.LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	set := Build(t.Context(), cfg, Options{Workspace: ws, Parts: Agents})
	if _, ok := set.Agents.Get("reviewer"); ok {
		t.Fatal("an untrusted workspace's definition is offered")
	}
	if _, ok := set.Agents.Get("mine"); !ok {
		t.Fatal("the operator's default directory was not read")
	}
	if ig := cfg.Workspace.IgnoredAgents(); len(ig) != 1 || ig[0] != "agents/reviewer" {
		t.Fatalf("the ignored definition is not named: %v", ig)
	}

	cfg, err = config.LoadWith(ws, config.LoadOptions{Trust: config.TrustGranted, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	set = Build(t.Context(), cfg, Options{Workspace: ws, Parts: Agents})
	if d, ok := set.Agents.Get("reviewer"); !ok || d.Source != "workspace" {
		t.Fatalf("a trusted workspace's definition is not offered: %+v", d)
	}

	// Without the Agents part only the built-in roles are offered.
	if set = Build(t.Context(), cfg, Options{Workspace: ws}); len(set.Agents.Loaded()) != 0 {
		t.Fatal("definitions loaded without the Agents part")
	}
}

// Only offered, configured providers may be named by a definition.
func TestOfferedModels(t *testing.T) {
	cfg := config.Default()
	cfg.Model.Providers["mine"] = config.ProviderConfig{Type: "openai-compatible", Model: "m"}
	cfg.SetKeys = append(cfg.SetKeys, "model.providers.mine")
	got := OfferedModels(cfg)
	if len(got) != 2 || got[0] != "local" || got[1] != "mine" {
		t.Fatalf("offered: %v", got)
	}
}

// An untrusted workspace file cannot add a provider, so no subagent can be
// sent to one it names; trusted, the name resolves.
func TestUntrustedConfigCannotAddSubagentModel(t *testing.T) {
	_, ws := agentsHome(t)
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := `{"model":{"providers":{"theirs":{"type":"openai-compatible","base_url":"http://127.0.0.1:9/v1","model":"x","context_window":8192}}}}`
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadWith(ws, config.LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ModelResolver(cfg)("theirs"); err == nil || !strings.Contains(err.Error(), "available: local") {
		t.Fatalf("an untrusted file's provider resolved: %v", err)
	}
	cfg, err = config.LoadWith(ws, config.LoadOptions{Trust: config.TrustGranted, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if a, err := ModelResolver(cfg)("theirs"); err != nil || a.Profile().Name != "x" {
		t.Fatalf("a trusted file's provider: %v", err)
	}
	for _, v := range []string{"http://127.0.0.1:9/v1", "local/x"} {
		if _, err := ModelResolver(cfg)(v); err == nil {
			t.Fatalf("%q resolved", v)
		}
	}
}

// A built-in provider the configuration never named is in the map but not
// offered, so a subagent cannot be sent to it.
func TestModelResolverOnlyOffered(t *testing.T) {
	cfg := config.Default()
	cfg.Model.Default = "mine"
	cfg.Model.Providers["mine"] = config.ProviderConfig{Type: "openai-compatible", BaseURL: "http://127.0.0.1:9/v1", Model: "m", ContextWindow: 8192}
	cfg.SetKeys = append(cfg.SetKeys, "model.providers.mine")
	if _, err := ModelResolver(cfg)("local"); err == nil || !strings.Contains(err.Error(), "available: mine") {
		t.Fatalf("an unoffered built-in resolved: %v", err)
	}
	if _, err := ModelResolver(cfg)("mine"); err != nil {
		t.Fatal(err)
	}
}

// The surface's ceiling caps the configured wake mode.
func TestBackgroundPolicyCeiling(t *testing.T) {
	cfg := config.Default()
	cfg.Subagents.Wake = "auto"
	for ceiling, want := range map[agent.WakeMode]agent.WakeMode{agent.WakeAuto: agent.WakeAuto, agent.WakeNotify: agent.WakeNotify, agent.WakeOff: agent.WakeOff} {
		if got := BackgroundPolicy(cfg, ceiling).Wake; got != want {
			t.Fatalf("ceiling %s gave %s", ceiling, got)
		}
	}
	cfg.Subagents.Wake = "off"
	if got := BackgroundPolicy(cfg, agent.WakeAuto).Wake; got != agent.WakeOff {
		t.Fatalf("configured off became %s", got)
	}
	cfg.Limits.BackgroundMaxMinutes = 10_000
	if p := BackgroundPolicy(cfg, agent.WakeAuto); p.Lifetime != 480*time.Minute || p.MaxLive != 4 {
		t.Fatalf("policy: %+v", p)
	}
}

// Subagents registers task_status and task_cancel only with background.
func TestBackgroundToolsOnlyWithBackground(t *testing.T) {
	for _, bg := range []bool{false, true} {
		f := &agent.SubagentFactory{Workspace: t.TempDir(), Background: bg}
		reg := Subagents(tools.NewRegistry(), f, 2)
		_, status := reg.Get("task_status")
		_, cancel := reg.Get("task_cancel")
		if status != bg || cancel != bg {
			t.Fatalf("background %v: task_status %v task_cancel %v", bg, status, cancel)
		}
	}
}
