package toolset

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
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
