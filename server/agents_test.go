package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
)

// isolateAgents gives the test its own home, trust store and managed
// definitions directory, and returns an operator directory to write into.
func isolateAgents(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(config.TrustEnv, "")
	old := managed.AgentsDir
	managed.AgentsDir = filepath.Join(t.TempDir(), "none")
	t.Cleanup(func() { managed.AgentsDir = old })
	return t.TempDir()
}

func putAgent(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An administrator reloads the definitions; nobody else can, and the reload
// is audited and reports what loaded and what did not.
func TestAdminReloadsAgents(t *testing.T) {
	dir := isolateAgents(t)
	g := newHookRig(t, nil, func(o *Options) { o.Config.Agents.Dirs = []string{dir} })
	alice, bob := g.signIn(t, "alice"), g.signIn(t, "bob")
	putAgent(t, dir, "reviewer.md", "---\ndescription: reviews\n---\nReview.\n")
	putAgent(t, dir, "explore.md", "---\ndescription: shadow the built-in\n---\nNo.\n")

	if rec := g.do(bob, "POST", "/v1/admin/agents/reload", ``); rec.Code != http.StatusForbidden {
		t.Fatalf("a non-administrator reloaded: %d", rec.Code)
	}
	rec := g.do(alice, "POST", "/v1/admin/agents/reload", ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("reload = %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Loaded   int      `json:"loaded"`
		Agents   []string `json:"agents"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Loaded != 1 || len(body.Agents) != 1 || body.Agents[0] != "reviewer" || len(body.Warnings) != 1 {
		t.Fatalf("reload answered %+v", body)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.audit) != 1 || g.audit[0].action != "agents.reloaded" {
		t.Fatalf("audit = %+v", g.audit)
	}
}
