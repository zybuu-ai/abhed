package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/managed"
)

const allowUser = `{"model": {"default": "claude",
  "providers": {
    "claude": {"type": "anthropic", "model": "claude-sonnet-4", "api_key": "k"},
    "gpt": {"type": "openai", "model": "gpt-4o", "api_key": "k"},
    "qwen": {"type": "ollama", "base_url": "http://127.0.0.1:11434", "model": "qwen3:8b"}
  },
  "fallback": ["qwen", "gpt"], "completion": "gpt", "commit": "claude"},
  "suggest": {"model": "gpt"}}`

func refusedBy(t *testing.T, err error, key string) *ManagedError {
	t.Helper()
	var me *ManagedError
	if !errors.As(err, &me) || me.Key != key {
		t.Fatalf("want a %s refusal, got %v", key, err)
	}
	return me
}

func TestGlob(t *testing.T) {
	for _, c := range []struct {
		pat, s string
		want   bool
	}{
		{"anthropic/claude-*", "anthropic/claude-sonnet-4", true},
		{"anthropic/claude-*", "openai/claude-sonnet-4", false},
		{"*/gpt-4o", "openai/gpt-4o", true},
		{"ollama/qwen3:*", "ollama/qwen3:8b", true},
		{"ollama/qwen3:*", "ollama/qwen2:8b", false},
		{"openai/gpt-4o", "openai/gpt-4o-mini", false},
		{"watsonx/*/gpt-oss-*", "watsonx/openai/gpt-oss-120b", true},
		{"*", "", true},
	} {
		if got := glob(c.pat, c.s); got != c.want {
			t.Errorf("glob(%q, %q) = %v", c.pat, c.s, got)
		}
	}
}

// The managed list admits only the models it names, on every lookup, and the
// load itself does not fail on a default it refuses.
func TestManagedModelAllowed(t *testing.T) {
	withManaged(t, `{"model": {"allowed": ["openai/gpt-*", "ollama/qwen3:*"]}}`)
	writeConfig(t, os.Getenv("HOME"), allowUser)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Provider(); err == nil {
		t.Fatal("the default claude is outside the list")
	} else {
		me := refusedBy(t, err, "model.allowed")
		if me.File != managed.ConfigFile || !strings.Contains(me.Value, "anthropic/claude-sonnet-4") {
			t.Fatalf("refusal: %+v", me)
		}
	}
	for name, want := range map[string]bool{"claude": false, "gpt": true, "qwen": true} {
		_, err := cfg.ProviderNamed(name)
		if (err == nil) != want || (cfg.ModelAllowed(name) == nil) != want || (cfg.PinnedAway(name) == nil) != want {
			t.Errorf("%s: ProviderNamed %v, ModelAllowed %v, PinnedAway %v", name, err, cfg.ModelAllowed(name), cfg.PinnedAway(name))
		}
	}
	if err := cfg.EndpointRefused(ProviderConfig{Type: "anthropic", Model: "claude-opus-4"}); err == nil {
		t.Error("an endpoint outside the list")
	}
	if err := cfg.EndpointRefused(ProviderConfig{Type: "openai", Model: "gpt-5"}); err != nil {
		t.Errorf("an endpoint in the list: %v", err)
	}
	// The editor's commit model is refused where it is asked for, and recorded.
	if _, err := cfg.ProviderNamed(cfg.Model.Commit); err == nil {
		t.Error("model.commit names claude")
	}
	keys := map[string]string{}
	for _, k := range cfg.SetAside {
		keys[k.Key] = k.Value
		if k.Key == "model.commit" && k.Layer != LayerManaged {
			t.Errorf("set aside for the managed list: %+v", k)
		}
	}
	if keys["model.commit"] != "claude" {
		t.Errorf("set aside: %v", cfg.SetAside)
	}
	if !slices.Equal(cfg.Model.Fallback, []string{"qwen", "gpt"}) || cfg.Suggest.Model != "gpt" {
		t.Errorf("admitted settings changed: %v %q", cfg.Model.Fallback, cfg.Suggest.Model)
	}
}

// A lower layer narrows the managed list and never widens it; a fallback, a
// suggestion or critic model the lists refuse is dropped and set aside.
func TestModelAllowedOnlyNarrows(t *testing.T) {
	withManaged(t, `{"model": {"allowed": ["openai/*", "anthropic/*"]}}`)
	user := strings.Replace(allowUser, `"fallback"`, `"allowed": ["anthropic/*", "ollama/*"], "fallback"`, 1)
	writeConfig(t, os.Getenv("HOME"), user)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Provider(); err != nil {
		t.Fatalf("claude is in both lists: %v", err)
	}
	refusedBy(t, cfg.ModelAllowed("gpt"), "model.allowed")  // the user's list leaves it out
	refusedBy(t, cfg.ModelAllowed("qwen"), "model.allowed") // the user may not widen the managed list
	if len(cfg.Model.Fallback) != 0 || cfg.Suggest.Model != "" {
		t.Errorf("refused fallback %v or suggest.model %q kept", cfg.Model.Fallback, cfg.Suggest.Model)
	}
	var aside []string
	for _, k := range cfg.SetAside {
		aside = append(aside, k.Key+"="+k.Value)
	}
	for _, want := range []string{"model.fallback=qwen", "model.fallback=gpt", "suggest.model=gpt", "model.completion=gpt"} {
		if !slices.Contains(aside, want) {
			t.Errorf("not set aside: %s (have %v)", want, aside)
		}
	}
	if got := len(cfg.ModelsAllowed()); got != 2 {
		t.Errorf("lists shown: %d", got)
	}
}

// A list that cannot be read stops the load: it fails closed.
func TestMalformedAllowListsFailClosed(t *testing.T) {
	for name, body := range map[string]string{
		"empty model list":     `{"model": {"allowed": []}}`,
		"no type":              `{"model": {"allowed": ["claude-*"]}}`,
		"whitespace":           `{"model": {"allowed": ["openai/gpt 4"]}}`,
		"not a list":           `{"model": {"allowed": "openai/*"}}`,
		"empty mcp entry":      `{"mcp": {"allowed": [{}]}}`,
		"unknown mcp field":    `{"mcp": {"denied": [{"names": "x"}]}}`,
		"bad digest":           `{"mcp": {"allowed": [{"digest": "sha256:abc"}]}}`,
		"relative command":     `{"mcp": {"allowed": [{"command": "bin/server"}]}}`,
		"host with scheme":     `{"mcp": {"allowed": [{"host": "https://mcp.example.com"}]}}`,
		"command and host":     `{"mcp": {"allowed": [{"command": "/bin/x", "host": "a.example"}]}}`,
		"mcp list not objects": `{"mcp": {"allowed": ["github"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			withManaged(t, body)
			if _, err := Load(t.TempDir()); err == nil {
				t.Fatal("loaded")
			}
			if _, err := LoadManaged(); err == nil {
				t.Fatal("LoadManaged loaded")
			}
		})
	}
	// The same in the user's own file stops the load too.
	withManaged(t, `{}`)
	writeConfig(t, os.Getenv("HOME"), `{"model": {"allowed": ["nope"]}}`)
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("a malformed user list loaded")
	}
}

func executable(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMCPAdmitted(t *testing.T) {
	bin := t.TempDir()
	gh := executable(t, bin, "github-mcp")
	executable(t, bin, "other-mcp")
	if err := os.Symlink(gh, filepath.Join(bin, "gh-link")); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(gh)
	digest := "sha256:" + strings.Repeat("a", 64)
	cfg := Default()
	cfg.MCP.Allowed = []MCPMatch{{Command: real}, {Host: "*.corp.example"}, {Name: "pinned", Digest: digest}}
	cfg.MCP.Denied = []MCPMatch{{Name: "bad-*"}}
	path := "PATH=" + bin
	for _, c := range []struct {
		s    MCPServerConfig
		want string // "" admitted, else the refusing key
	}{
		{MCPServerConfig{Name: "github", Command: "github-mcp", Env: []string{path}}, ""},
		{MCPServerConfig{Name: "github", Command: real}, ""},
		{MCPServerConfig{Name: "linked", Command: "gh-link", Env: []string{path}}, ""},
		{MCPServerConfig{Name: "other", Command: "other-mcp", Env: []string{path}}, "mcp.allowed"},
		{MCPServerConfig{Name: "missing", Command: "nowhere-mcp", Env: []string{path}}, "mcp.allowed"},
		{MCPServerConfig{Name: "bad-github", Command: real}, "mcp.denied"},
		{MCPServerConfig{Name: "remote", URL: "https://mcp.corp.example/sse"}, ""},
		{MCPServerConfig{Name: "remote", URL: "https://corp.example.evil.test/sse"}, "mcp.allowed"},
		{MCPServerConfig{Name: "remote", URL: "https://MCP.Corp.Example./x"}, ""},
		{MCPServerConfig{Name: "pinned", Command: "other-mcp", Env: []string{path}, Digest: digest}, ""},
		{MCPServerConfig{Name: "pinned", Command: "other-mcp", Env: []string{path}}, "mcp.allowed"},
		{MCPServerConfig{Name: "unpinned", Command: "other-mcp", Env: []string{path}, Digest: digest}, "mcp.allowed"},
	} {
		err := cfg.MCPAdmitted(MCPServerOf(c.s))
		if c.want == "" {
			if err != nil {
				t.Errorf("%+v: %v", c.s, err)
			}
			continue
		}
		refusedBy(t, err, c.want)
	}
}

// Servers the lists refuse are taken out at load and set aside with why; a
// lower layer narrows; a managed mcp.allowed leaves the servers the user's.
func TestMCPListsAtLoad(t *testing.T) {
	withManaged(t, `{"mcp": {"allowed": [{"name": "docs"}, {"name": "search"}, {"host": "mcp.corp.example"}], "denied": [{"name": "search"}]}}`)
	writeConfig(t, os.Getenv("HOME"), `{"mcp": {"denied": [{"name": "docs"}], "servers": [
	  {"name": "docs", "command": "/bin/echo", "enabled": true},
	  {"name": "search", "url": "https://search.example/mcp", "enabled": true},
	  {"name": "wiki", "url": "https://mcp.corp.example/mcp", "enabled": true},
	  {"name": "shell", "command": "/bin/sh", "enabled": true}]}}`)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range cfg.MCP.Servers {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"wiki"}) {
		t.Fatalf("servers kept: %v", names)
	}
	why := map[string]string{}
	for _, k := range cfg.SetAside {
		if k.Key == "mcp.servers" {
			why[k.Value] = k.Layer + ": " + k.Reason
		}
	}
	for name, want := range map[string]string{"docs": "user", "search": "managed", "shell": "managed"} {
		if !strings.HasPrefix(why[name], want+":") {
			t.Errorf("%s set aside as %q, want layer %s", name, why[name], want)
		}
	}
	if cfg.ManagedSets("mcp.servers") {
		t.Error("a managed mcp.allowed is not a managed mcp.servers")
	}
	if len(cfg.MCPLists()) != 3 {
		t.Errorf("lists: %v", cfg.MCPLists())
	}
}

// An untrusted workspace file's lists apply as a narrowing: they take models
// and servers away and never add one; a malformed one is ignored, not fatal.
func TestUntrustedWorkspaceListsOnlyNarrow(t *testing.T) {
	withManaged(t, `{"model": {"allowed": ["openai/*", "anthropic/*"]}}`)
	writeConfig(t, os.Getenv("HOME"), allowUser)
	ws := t.TempDir()
	writeConfig(t, ws, `{"model": {"allowed": ["anthropic/*", "ollama/*"]}, "mcp": {"denied": [{"name": "docs"}]}}`)
	cfg, err := LoadWith(ws, LoadOptions{Trust: TrustRefused, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.Trusted {
		t.Fatal("the workspace is trusted")
	}
	if cfg.ModelAllowed("claude") != nil {
		t.Errorf("claude is in both lists: %v", cfg.ModelAllowed("claude"))
	}
	refusedBy(t, cfg.ModelAllowed("gpt"), "model.allowed")  // taken away by the workspace
	refusedBy(t, cfg.ModelAllowed("qwen"), "model.allowed") // not added by it
	refusedBy(t, cfg.MCPAdmitted(MCPServer{Name: "docs", Programs: []string{"/bin/docs"}}), "mcp.denied")
	for _, k := range []string{"model.allowed", "mcp.denied"} {
		if !slices.Contains(cfg.Workspace.Applied, k) {
			t.Errorf("%s not applied: %v", k, cfg.Workspace.Applied)
		}
	}
	var layers []string
	for _, l := range cfg.ModelsAllowed() {
		layers = append(layers, l.Layer)
	}
	if !slices.Equal(layers, []string{LayerWorkspace, LayerManaged}) && !slices.Equal(layers, []string{LayerManaged, LayerWorkspace}) {
		t.Errorf("layers: %v", layers)
	}

	// A malformed list there is ignored, and the session still starts.
	writeConfig(t, ws, `{"model": {"allowed": ["no-slash"]}, "mcp": {"allowed": [{}]}}`)
	cfg, err = LoadWith(ws, LoadOptions{Trust: TrustRefused, Quiet: true})
	if err != nil {
		t.Fatalf("a malformed untrusted list stopped the load: %v", err)
	}
	if cfg.ModelAllowed("gpt") != nil || slices.Contains(cfg.Workspace.Applied, "model.allowed") {
		t.Errorf("the malformed list applied: %v %v", cfg.ModelAllowed("gpt"), cfg.Workspace.Applied)
	}
	ignored := map[string]bool{}
	for _, k := range cfg.Workspace.Ignored {
		ignored[k.Key] = true
	}
	if !ignored["model.allowed"] || !ignored["mcp.allowed"] {
		t.Errorf("not reported as ignored: %+v", cfg.Workspace.Ignored)
	}

	// Trusted, the same lists narrow the same way.
	writeConfig(t, ws, `{"model": {"allowed": ["anthropic/*", "ollama/*"]}}`)
	cfg, err = LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	refusedBy(t, cfg.ModelAllowed("gpt"), "model.allowed")
	refusedBy(t, cfg.ModelAllowed("qwen"), "model.allowed")
}
