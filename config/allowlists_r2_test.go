package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// loadAllow loads with a managed body and a user body, under a fresh HOME.
func loadAllow(t *testing.T, managedBody, user string) Config {
	t.Helper()
	withManaged(t, managedBody)
	writeConfig(t, os.Getenv("HOME"), user)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

const userDefault = `"default": "claude", "providers": {"claude": {"type": "anthropic", "model": "claude-sonnet-4", "api_key": "k"}`

// A vertex type writes its region and project into the host: under the
// organisation's list they must be plain names.
func TestVertexRegionCannotMoveTheHost(t *testing.T) {
	cfg := loadAllow(t, `{"model": {"allowed": ["vertex-anthropic/claude-*"]}}`,
		`{"model": {`+userDefault+`,
		  "bad": {"type": "vertex-anthropic", "region": "evil.example/x?", "project_id": "p", "model": "claude-sonnet-4"},
		  "badproj": {"type": "vertex-anthropic", "region": "us-east5", "project_id": "p@evil.example", "model": "claude-sonnet-4"},
		  "good": {"type": "vertex-anthropic", "region": "us-east5", "project_id": "my-proj-1", "model": "claude-sonnet-4"}}}}`)
	for name, ok := range map[string]bool{"bad": false, "badproj": false, "good": true} {
		if err := cfg.ModelAllowed(name); (err == nil) != ok {
			t.Errorf("%s: %v", name, err)
		}
	}
	// And the adapter itself refuses such a region, list or no list.
	if _, err := (ProviderConfig{Type: "vertex-anthropic", Region: "evil.example/x?", ProjectID: "p", Model: "m"}).Adapter(); err == nil {
		t.Error("the vertex adapter took a region that is not a plain name")
	}
	if _, err := (ProviderConfig{Type: "vertex-gemini", Region: "us-central1", ProjectID: "a/b", Model: "m"}).Adapter(); err == nil {
		t.Error("the vertex-gemini adapter took a project that is not a plain name")
	}
}

// A custom provider type carries its own base_url: under the organisation's
// list it is admitted only when the managed file defines the custom type, or
// its endpoint is on this machine.
func TestCustomProviderTypeIsBound(t *testing.T) {
	user := `{"custom_providers": [{"name": "relay", "api": "anthropic", "base_url": "https://relay.evil.example"},
	  {"name": "box", "api": "openai", "base_url": "http://127.0.0.1:4000/v1"}],
	  "model": {` + userDefault + `,
	  "r": {"type": "relay", "model": "claude-sonnet-4"},
	  "b": {"type": "box", "model": "claude-sonnet-4"}}}}`
	cfg := loadAllow(t, `{"model": {"allowed": ["*/claude-sonnet-4"]}}`, user)
	me := refusedBy(t, cfg.ModelAllowed("r"), "model.allowed")
	if !strings.Contains(me.Reason, "custom provider") || !strings.Contains(me.Reason, "relay.evil.example") {
		t.Errorf("reason %q", me.Reason)
	}
	if err := cfg.ModelAllowed("b"); err != nil {
		t.Errorf("a custom type on this machine: %v", err)
	}
	cfg = loadAllow(t, `{"model": {"allowed": ["*/claude-sonnet-4"]},
	  "custom_providers": [{"name": "relay", "api": "anthropic", "base_url": "https://relay.corp.example"}]}`, user)
	if err := cfg.ModelAllowed("r"); err != nil {
		t.Errorf("a custom type the managed file defines: %v", err)
	}
}

// The supervisor judge's endpoint is bound under the organisation's list.
func TestJudgeEndpointIsBound(t *testing.T) {
	judge := func(endpoint string) MonitorConfig {
		return MonitorConfig{Enabled: true, Judge: "model", Endpoint: endpoint, Model: "claude-sonnet-4"}
	}
	cfg := loadAllow(t, `{"model": {"allowed": ["*/claude-sonnet-4"]}}`, `{"model": {`+userDefault+`}}}`)
	me := refusedBy(t, cfg.JudgeListed(judge("https://judge.evil.example/v1")), "model.allowed")
	if !strings.Contains(me.Reason, "judge.evil.example") {
		t.Errorf("reason %q", me.Reason)
	}
	if err := cfg.JudgeListed(judge("http://127.0.0.1:8861/v1")); err != nil {
		t.Errorf("a judge on this machine: %v", err)
	}
	// Loaded from the user's file, the judge is set aside and named.
	cfg = loadAllow(t, `{"model": {"allowed": ["*/claude-sonnet-4"]}}`, `{"model": {`+userDefault+`}},
	  "monitor": {"enabled": true, "judge": "model", "endpoint": "https://judge.evil.example/v1", "model": "claude-sonnet-4"}}`)
	found := false
	for _, k := range cfg.SetAside {
		found = found || k.Key == "monitor.model" && strings.Contains(k.Reason, "judge.evil.example")
	}
	if !found {
		t.Errorf("the judge was not set aside: %v", cfg.SetAside)
	}
	cfg = loadAllow(t, `{"model": {"allowed": ["*/claude-sonnet-4"]},
	  "monitor": {"enabled": true, "judge": "model", "endpoint": "https://judge.corp.example/v1", "model": "claude-sonnet-4"}}`, `{"model": {`+userDefault+`}}}`)
	if err := cfg.JudgeListed(cfg.Monitor); err != nil {
		t.Errorf("a judge endpoint the managed file sets: %v", err)
	}
	// A user's own list binds no endpoint, the judge's included.
	plain := Default()
	plain.Model.Allowed = []string{"*/claude-sonnet-4"}
	if err := plain.JudgeListed(judge("https://judge.example/v1")); err != nil {
		t.Errorf("a user's list bound the judge's endpoint: %v", err)
	}
}

// watsonx sends its key for a token to iam_url: bound like base_url.
func TestIAMURLIsBound(t *testing.T) {
	cfg := loadAllow(t, `{"model": {"allowed": ["watsonx/*"]}}`, `{"model": {`+userDefault+`,
	  "wx": {"type": "watsonx", "base_url": "http://127.0.0.1:9/", "model": "m", "api_key": "k", "project_id": "p", "iam_url": "https://iam.evil.example/token"},
	  "wx2": {"type": "watsonx", "base_url": "http://127.0.0.1:9/", "model": "m", "api_key": "k", "project_id": "p", "extra": {"iam_url": "https://iam.evil.example/token"}},
	  "wx3": {"type": "watsonx", "base_url": "http://127.0.0.1:9/", "model": "m", "api_key": "k", "project_id": "p"}}}}`)
	refusedBy(t, cfg.ModelAllowed("wx"), "model.allowed")
	refusedBy(t, cfg.ModelAllowed("wx2"), "model.allowed")
	if err := cfg.ModelAllowed("wx3"); err != nil {
		t.Errorf("no iam_url: %v", err)
	}
}

// model.allowed_loopback false refuses an endpoint on this machine, a type
// whose own endpoint is local included, but not a provider the managed file
// defines there.
func TestAllowedLoopbackFalse(t *testing.T) {
	cfg := loadAllow(t, `{"model": {"allowed": ["*/*"], "allowed_loopback": false,
	  "providers": {"corp": {"type": "openai-compatible", "base_url": "http://127.0.0.1:4000/v1", "model": "c"}}}}`,
		`{"model": {`+userDefault+`,
	  "relay": {"type": "anthropic", "base_url": "http://127.0.0.1:4000", "model": "claude-sonnet-4"},
	  "llama": {"type": "ollama", "model": "qwen3:8b"}}}}`)
	for name, ok := range map[string]bool{"claude": true, "relay": false, "llama": false, "corp": true} {
		if err := cfg.ModelAllowed(name); (err == nil) != ok {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := cfg.ProviderNamed("local"); err == nil {
		t.Error("the built-in local provider on this machine was admitted")
	}
}

// Numeric host forms other than a plain IP are not judged as names.
func TestNumericHostFormsAreRefused(t *testing.T) {
	cfg := Default()
	cfg.MCP.Denied = []MCPMatch{{Host: "127.0.0.1"}}
	for _, u := range []string{"http://2130706433/", "http://0x7f.1/", "http://127.1/", "http://0x7f000001/"} {
		refusedBy(t, cfg.MCPAdmitted(MCPServer{Name: "n", URL: u}), "mcp.servers")
	}
	refusedBy(t, cfg.MCPAdmitted(MCPServer{Name: "n", URL: "http://127.0.0.1/"}), "mcp.denied")
	if err := cfg.MCPAdmitted(MCPServer{Name: "n", URL: "http://host1.example/"}); err != nil {
		t.Errorf("a name with digits: %v", err)
	}
}

// The org policy's list binds every endpoint input, but only the machine's
// managed file defines one: an org policy's custom provider and judge
// endpoint are set aside, so neither is taken as bound. Its loopback switch
// may narrow (false) and never widen.
func TestOrgPolicyBindsEveryEndpointInput(t *testing.T) {
	noManaged(t)
	f := newOrg(t)
	f.write(t, `{"model":{"allowed":["*/claude-sonnet-4"],"allowed_loopback":false},
	  "custom_providers":[{"name":"orgrelay","api":"anthropic","base_url":"https://relay.corp.example"}],
	  "monitor":{"enabled":true,"judge":"model","endpoint":"https://judge.corp.example/v1","model":"claude-sonnet-4"}}`, time.Now())
	writeConfig(t, os.Getenv("HOME"), `{"model": {`+userDefault+`,
	  "o": {"type": "orgrelay", "model": "claude-sonnet-4"},
	  "lo": {"type": "anthropic", "base_url": "http://127.0.0.1:4000", "model": "claude-sonnet-4"}}},
	  "custom_providers":[{"name":"orgrelay","api":"anthropic","base_url":"https://relay.corp.example"}],
	  "monitor":{"judge":"model","endpoint":"https://judge.corp.example/v1","model":"claude-sonnet-4"}}`)
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FromOrg("custom_providers") || cfg.FromOrg("monitor.endpoint") || cfg.machineSets("custom_providers") {
		t.Fatal("an org policy's endpoint input was taken")
	}
	// The same values from the person's file are not bound by the org's list.
	refusedBy(t, cfg.ModelAllowed("o"), "model.allowed")
	refusedBy(t, cfg.JudgeListed(cfg.Monitor), "model.allowed")
	// allowed_loopback false from the org narrows.
	refusedBy(t, cfg.ModelAllowed("lo"), "model.allowed")
}

// An org policy may never turn allowed_loopback on.
func TestOrgPolicyCannotWidenLoopback(t *testing.T) {
	withManaged(t, `{"model":{"allowed":["*/claude-sonnet-4"],"allowed_loopback":false}}`)
	f := newOrg(t)
	f.write(t, `{"model":{"allowed_loopback":true}}`, time.Now())
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoopbackAllowed() {
		t.Fatal("the org policy turned loopback back on")
	}
	noManaged(t)
	f.write(t, `{"model":{"allowed":["*/claude-sonnet-4"],"allowed_loopback":true}}`, time.Now())
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FromOrg("model.allowed_loopback") {
		t.Error("allowed_loopback true from the org was taken")
	}
}

// A digest that pins an engine's image is admitted by an allow entry only with
// the engine's path named in the same entry; a deny entry by digest alone
// still matches it.
func TestImagePinnedServers(t *testing.T) {
	d := "sha256:" + strings.Repeat("c", 64)
	srv := MCPServerOf(MCPServerConfig{Name: "github", Command: "/Users/me/x/docker", Args: []string{"run", "a@" + d}, Digest: d})
	if !srv.ImagePinned {
		t.Fatal("an engine's image pin was not seen")
	}
	srv.Programs = []string{"/Users/me/x/docker"}
	cfg := Default()
	cfg.MCP.Allowed = []MCPMatch{{Name: "github", Digest: d}}
	refusedBy(t, cfg.MCPAdmitted(srv), "mcp.allowed")
	cfg.MCP.Allowed = []MCPMatch{{Name: "github", Digest: d, Command: "/usr/local/bin/docker"}}
	refusedBy(t, cfg.MCPAdmitted(srv), "mcp.allowed")
	srv.Programs = []string{"/usr/local/bin/docker"}
	if err := cfg.MCPAdmitted(srv); err != nil {
		t.Fatalf("the engine's path and the image: %v", err)
	}
	// A file digest is not an image pin: a digest-only entry admits it.
	file := MCPServerOf(MCPServerConfig{Name: "github", Command: "/opt/gh-mcp", Digest: d})
	if file.ImagePinned {
		t.Fatal("a file digest read as an image pin")
	}
	cfg.MCP.Allowed = []MCPMatch{{Name: "github", Digest: d}}
	if err := cfg.MCPAdmitted(file); err != nil {
		t.Fatalf("a file digest: %v", err)
	}
	cfg.MCP.Allowed, cfg.MCP.Denied = nil, []MCPMatch{{Digest: d}}
	refusedBy(t, cfg.MCPAdmitted(srv), "mcp.denied")
}
