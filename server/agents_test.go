package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/tools"
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

// offeredTypes is what a session's task tool offers.
func offeredTypes(t *testing.T, s *Server, id string) []string {
	t.Helper()
	s.mu.Lock()
	live := s.running[id]
	s.mu.Unlock()
	if live == nil {
		t.Fatalf("session %s is not running here", id)
	}
	tk, ok := live.Loop.Tools.Get("task")
	if !ok {
		t.Fatal("the session has no task tool")
	}
	return tk.(agent.Task).Agents.Names()
}

func startSession(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"hello"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	return created.SessionID
}

// A reload reaches the sessions started after it; a session already running
// keeps the agent types its prompt and record say it was offered.
func TestAgentReloadReachesNewSessionsOnly(t *testing.T) {
	dir := isolateAgents(t)
	cfg := config.Default()
	cfg.Agents.Dirs = []string{dir}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{})})
	before := startSession(t, s)

	putAgent(t, dir, "auditor.md", "---\ndescription: audits\n---\nAudit.\n")
	rec := httptest.NewRecorder()
	s.reloadAgents(rec, httptest.NewRequest("POST", "/v1/admin/agents/reload", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("reload: %d %s", rec.Code, rec.Body)
	}
	after := startSession(t, s)

	if got := offeredTypes(t, s, before); slices.Contains(got, "auditor") {
		t.Fatalf("a running session's types changed on reload: %v", got)
	}
	if got := offeredTypes(t, s, after); !slices.Contains(got, "auditor") {
		t.Fatalf("a session started after the reload lacks the definition: %v", got)
	}
}

// A subagent may run only on a provider this server offers sessions: a
// built-in the configuration never named is refused, and so is an endpoint.
func TestServerOfferedGatesSubagentModel(t *testing.T) {
	isolateAgents(t)
	cfg := config.Default()
	cfg.Model.Default = "mine"
	cfg.Model.Providers["mine"] = config.ProviderConfig{Type: "openai-compatible", BaseURL: "http://127.0.0.1:9/v1", Model: "m", ContextWindow: 8192}
	cfg.Model.Providers["other"] = config.ProviderConfig{Type: "openai-compatible", BaseURL: "http://127.0.0.1:9/v1", Model: "o", ContextWindow: 8192}
	cfg.SetKeys = append(cfg.SetKeys, "model.providers.mine", "model.providers.other")
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{})})
	if a, err := s.subagentModel("other"); err != nil || a.Profile().Name != "o" {
		t.Fatalf("an offered provider: %v", err)
	}
	for _, name := range []string{"local", "nope", "http://127.0.0.1:9/v1"} {
		_, err := s.subagentModel(name)
		if err == nil || !strings.Contains(err.Error(), "available: mine, other") {
			t.Fatalf("%q: %v", name, err)
		}
	}
	id := startSession(t, s)
	s.mu.Lock()
	live := s.running[id]
	s.mu.Unlock()
	tk, _ := live.Loop.Tools.Get("task")
	if got := tk.(agent.Task).Models; len(got) != 2 {
		t.Fatalf("the session's task tool offers models %v", got)
	}
	if live.Loop.Provider != "mine" {
		t.Fatalf("the session's provider is %q", live.Loop.Provider)
	}
}
