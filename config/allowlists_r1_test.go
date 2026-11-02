package config

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Hosts are judged in the form a connection dials: a fullwidth or
// otherwise mapped name matches the ASCII entry, so a deny cannot be slipped
// past, and a host that does not map is refused under any list.
func TestMCPHostsAreNormalised(t *testing.T) {
	cfg := Default()
	cfg.MCP.Denied = []MCPMatch{{Host: "evil.com"}, {Host: "*.bad.example"}}
	for _, u := range []string{
		"https://ｅｖｉｌ.com/mcp", "https://EVIL.com/mcp", "https://evil。com/mcp", "https://evil.com./mcp",
		"https://x.ｂａｄ.example/mcp", "https://\u200bevil.com/mcp",
	} {
		refusedBy(t, cfg.MCPAdmitted(MCPServer{Name: "r", URL: u}), "mcp.denied")
	}
	// A mixed-script name is a different host: it is not the denied one,
	// and an allow list does not admit it either.
	if err := cfg.MCPAdmitted(MCPServer{Name: "r", URL: "https://еvil.com/mcp"}); err != nil {
		t.Errorf("a different host was denied: %v", err)
	}
	cfg.MCP.Allowed = []MCPMatch{{Host: "mcp.corp.example"}}
	refusedBy(t, cfg.MCPAdmitted(MCPServer{Name: "r", URL: "https://mcp.соrp.example/x"}), "mcp.allowed")
	if err := cfg.MCPAdmitted(MCPServer{Name: "r", URL: "https://ＭＣＰ.corp.example/x"}); err != nil {
		t.Errorf("the fullwidth form of the allowed host: %v", err)
	}
	// A host that does not map is refused outright.
	refusedBy(t, cfg.MCPAdmitted(MCPServer{Name: "r", URL: "https://a\u0000b.example/x"}), "mcp.servers")
	// An entry written in Unicode is read in the same form.
	cfg.MCP.Denied = []MCPMatch{{Host: "bücher.example"}}
	refusedBy(t, cfg.MCPAdmitted(MCPServer{Name: "r", URL: "https://xn--bcher-kva.example/"}), "mcp.denied")
	if err := checkMCPMatch(MCPMatch{Host: "bad\u0000host"}); err == nil {
		t.Error("an entry host that does not map was taken")
	}
}

// Under the organisation's model list, a model is admitted only at its
// type's own endpoint, on this machine, or at a provider the managed file
// defines: a user's base_url cannot send an admitted model anywhere.
func TestModelAllowedBindsTheEndpoint(t *testing.T) {
	withManaged(t, `{"model": {"allowed": ["anthropic/claude-*", "openai-compatible/*"],
	  "providers": {"corp": {"type": "openai-compatible", "base_url": "https://llm.corp.example/v1", "model": "m"}}}}`)
	writeConfig(t, os.Getenv("HOME"), `{"model": {"default": "claude", "providers": {
	  "claude": {"type": "anthropic", "model": "claude-sonnet-4", "api_key": "k"},
	  "relay": {"type": "anthropic", "base_url": "https://relay.evil.example", "model": "claude-sonnet-4", "api_key": "k"},
	  "local": {"type": "openai-compatible", "base_url": "http://127.0.0.1:11434/v1", "model": "qwen3"},
	  "mine": {"type": "openai-compatible", "base_url": "https://mine.example/v1", "model": "x"}}}}`)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, ok := range map[string]bool{"claude": true, "relay": false, "local": true, "mine": false, "corp": true} {
		err := cfg.ModelAllowed(name)
		if (err == nil) != ok {
			t.Errorf("%s: %v", name, err)
		}
		if !ok {
			me := refusedBy(t, err, "model.allowed")
			if !strings.Contains(me.Reason, "endpoint") {
				t.Errorf("%s: reason %q", name, me.Reason)
			}
		}
	}
	// The environment cannot move a provider off its own endpoint either.
	t.Setenv("ABHED_BASE_URL", "https://elsewhere.example")
	cfg, err = Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	refusedBy(t, func() error { _, err := cfg.Provider(); return err }(), "model.allowed")
	// An embedder's endpoint: refused off this machine, admitted on it.
	if err := cfg.EndpointRefused(ProviderConfig{Type: "anthropic", Model: "claude-x", BaseURL: "https://relay.example"}); err == nil {
		t.Error("an embedder's remote endpoint was admitted")
	}
	if err := cfg.EndpointRefused(ProviderConfig{Type: "anthropic", Model: "claude-x"}); err != nil {
		t.Errorf("the type's own endpoint: %v", err)
	}
}

// A user's own list judges type and model only: it binds no endpoint.
func TestUserModelListDoesNotBindTheEndpoint(t *testing.T) {
	cfg := Default()
	cfg.Model.Allowed = []string{"anthropic/*"}
	if err := cfg.ModelListed("x", ProviderConfig{Type: "anthropic", Model: "c", BaseURL: "https://relay.example"}); err != nil {
		t.Fatalf("a user's list bound the endpoint: %v", err)
	}
}

// The org policy's model list binds the endpoint as the managed file's does;
// a provider the org policy defines is admitted at its endpoint.
func TestOrgModelListBindsTheEndpoint(t *testing.T) {
	noManaged(t)
	f := newOrg(t)
	f.write(t, `{"model":{"allowed":["openai-compatible/*"],"providers":{"corp":{"type":"openai-compatible","base_url":"https://llm.corp.example/v1","model":"m"}}}}`, time.Now())
	writeConfig(t, os.Getenv("HOME"), `{"model":{"providers":{"mine":{"type":"openai-compatible","base_url":"https://mine.example/v1","model":"x"}}}}`)
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ModelAllowed("corp"); err != nil {
		t.Errorf("the org's provider: %v", err)
	}
	me := refusedBy(t, cfg.ModelAllowed("mine"), "model.allowed")
	if !strings.Contains(me.Reason, "org policy") {
		t.Errorf("reason %q", me.Reason)
	}
}

// A model id that could step out of its place in a request path is refused.
func TestModelIDsThatEscapeThePathAreRefused(t *testing.T) {
	cfg := Default()
	cfg.Model.Allowed = []string{"gemini/gemini-2.5-flash*"}
	for _, m := range []string{"gemini-2.5-flash/../gemini-ultra", "gemini-2.5-flash?x=1", "gemini-2.5-flash#a", "gemini-2.5-flash%2F..", `gemini-2.5-flash\..`} {
		refusedBy(t, cfg.ModelListed("g", ProviderConfig{Type: "gemini", Model: m}), "model.allowed")
	}
	if err := cfg.ModelListed("g", ProviderConfig{Type: "gemini", Model: "gemini-2.5-flash-lite"}); err != nil {
		t.Fatal(err)
	}
}

// A refusal of an unconfigured name is credited to the layer that set the list.
func TestUnconfiguredRefusalNamesTheListsFile(t *testing.T) {
	withManaged(t, `{}`)
	writeConfig(t, os.Getenv("HOME"), `{"model": {"allowed": ["anthropic/*"]}}`)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var me *ManagedError
	if err := cfg.PinnedAway("nosuch"); !errors.As(err, &me) || !strings.HasSuffix(me.File, ".abhed/config.json") {
		t.Fatalf("refusal: %+v", me)
	}
}

// An untrusted workspace's list is judged on its own, and a refusal it causes
// names the file and how to lift it.
func TestUntrustedListsAreJudgedOneByOne(t *testing.T) {
	withManaged(t, `{}`)
	writeConfig(t, os.Getenv("HOME"), allowUser)
	ws := t.TempDir()
	writeConfig(t, ws, `{"model": {"allowed": ["x/none"]}, "mcp": {"allowed": [{}], "denied": [{"name": "docs"}]}}`)
	cfg, err := LoadWith(ws, LoadOptions{Trust: TrustRefused, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	refusedBy(t, cfg.MCPAdmitted(MCPServer{Name: "docs"}), "mcp.denied") // kept though mcp.allowed was malformed
	me := refusedBy(t, func() error { _, err := cfg.Provider(); return err }(), "model.allowed")
	if !strings.Contains(me.Reason, "workspace file") || !strings.Contains(me.Reason, "take the list out of that file") {
		t.Errorf("reason %q", me.Reason)
	}
}
