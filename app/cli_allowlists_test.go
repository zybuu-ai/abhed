package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

func allowedCfg() config.Config {
	cfg := config.Default()
	cfg.Model.Default = "a"
	cfg.Model.Providers = map[string]config.ProviderConfig{
		"a": {Type: "openai-compatible", BaseURL: "http://127.0.0.1:9/v1", Model: "ok-1"},
		"b": {Type: "openai-compatible", BaseURL: "http://127.0.0.1:9/v1", Model: "no-1"},
		"c": {Type: "openai-compatible", BaseURL: "http://127.0.0.1:9/v1", Model: "ok-2"}}
	cfg.SetKeys = []string{"model.providers.a", "model.providers.b", "model.providers.c"}
	cfg.Model.Allowed = []string{"openai-compatible/ok-*"}
	return cfg
}

// /model, -model and the fallback chain hold the allowed models; a refused
// /model is recorded with the reason.
func TestCLIAllowedModels(t *testing.T) {
	cfg := allowedCfg()
	e, _ := configEnv(cfg, "")
	err := switchModel(context.Background(), e, "b")
	if err == nil || !strings.Contains(err.Error(), "model.allowed") {
		t.Fatalf("/model b: %v", err)
	}
	if e.st.appCfg.Model.Default != "a" {
		t.Fatal("the default moved")
	}
	if len(e.st.pending) != 1 {
		t.Fatalf("held events: %+v", e.st.pending)
	}
	a, ok := e.st.pending[0].payload.(agent.ConfigAttempt)
	if e.st.pending[0].typ != agent.EvConfigRefused || !ok || a.Key != "model.allowed" || a.Source != "/model" ||
		!strings.Contains(a.Value, "openai-compatible/no-1") || a.Decision != "refused" {
		t.Fatalf("the refusal: %+v", e.st.pending[0])
	}
	if err := switchModel(context.Background(), e, "c"); err != nil {
		t.Fatalf("/model c: %v", err)
	}
	if items := modelItems(e.st); len(items) != 2 {
		t.Fatalf("/model offers %v", items)
	}
	if _, err := modelFlag(cfg, "b"); err == nil || !strings.Contains(err.Error(), "model.allowed") {
		t.Fatalf("-model b: %v", err)
	}
	if got, err := modelFlag(cfg, "c"); err != nil || got.Model.Default != "c" {
		t.Fatalf("-model c: %v", err)
	}
	chain, warn := fallbackChain(cfg, "b,c")
	if !slices.Equal(chain, []string{"c"}) || len(warn) != 1 || !strings.Contains(warn[0], "model.allowed") {
		t.Fatalf("chain %q warn %q", chain, warn)
	}
}

// doctor and /permissions show each list with where it came from, and what it left out.
func TestAllowListLines(t *testing.T) {
	cfg := allowedCfg()
	cfg.MCP.Denied = []config.MCPMatch{{Name: "shell"}}
	cfg.SetAside = []config.SetAsideKey{{Key: "mcp.servers", Value: "shell", Reason: "denied"}, {Key: "model.fallback", Value: "b", Reason: "not listed"}}
	got := strings.Join(allowListLines(cfg), "\n")
	for _, want := range []string{"models      only openai-compatible/ok-*", "mcp deny    {name=shell}", "MCP server shell is not started: denied",
		"model.fallback b is not used: not listed"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if lines := allowListLines(config.Default()); len(lines) != 0 {
		t.Fatalf("no lists: %v", lines)
	}
}

// doctor shows the lists, the server they leave out, and a default model they
// refuse, which it does not probe.
func TestDoctorShowsTheAllowLists(t *testing.T) {
	m := &bgModelServer{}
	url := m.start(t)
	ws := bgWorkspace(t, url, `,"mcp":{"denied":[{"name":"shell"}],"servers":[{"name":"shell","command":"/bin/sh","enabled":true}]}`)
	var out bytes.Buffer
	cmd := mainHelper([]string{"-C", ws, "doctor"})
	cmd.Stdout, cmd.Stderr = &out, &out
	_ = cmd.Run()
	for _, want := range []string{"mcp deny    {name=shell}", "MCP server shell is not started", "mcp.denied"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor does not say %q:\n%s", want, out.String())
		}
	}

	home := os.Getenv("HOME")
	cfg := `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","allowed":["anthropic/*"],"providers":{"stub":{"type":"openai-compatible","base_url":"` +
		url + `","model":"m","context_window":8192}}}}`
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	before := m.calls.Load()
	cmd = mainHelper([]string{"-C", ws, "doctor"})
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err == nil {
		t.Fatalf("doctor passed a refused model:\n%s", out.String())
	}
	for _, want := range []string{"models      only anthropic/*", "model.allowed stub (openai-compatible/m) is refused", "not probed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor does not say %q:\n%s", want, out.String())
		}
	}
	if m.calls.Load() != before {
		t.Error("the refused model was probed")
	}
}

// -mcp-config and `abhed mcp add` refuse a server the MCP lists refuse, with why.
func TestMCPListsRefuseAddedServers(t *testing.T) {
	cfg := config.Default()
	cfg.MCP.Denied = []config.MCPMatch{{Name: "shell"}}
	file := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(file, []byte(`{"mcpServers":{"shell":{"command":"/bin/sh"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mcpFlags(cfg, flagFiles{}, []string{file}, false); err == nil || !strings.Contains(err.Error(), "mcp.denied") {
		t.Fatalf("-mcp-config: %v", err)
	}

	managedConfig(t, `{"mcp":{"allowed":[{"host":"mcp.corp.example"}]}}`)
	code, out := mcpRun(t, true, "y\n", "add", "docs", "/usr/bin/docs-mcp")
	if code == 0 || !strings.Contains(out, "mcp.allowed") {
		t.Fatalf("mcp add: %d %s", code, out)
	}
	if cfg, _ := userFileConfig(); len(cfg.MCP.Servers) != 0 {
		t.Fatalf("written: %+v", cfg.MCP)
	}
}
