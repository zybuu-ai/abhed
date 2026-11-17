package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #19: a server's readOnlyHint is believed only when the managed file defines
// it and opts it in; a
// server the person added, or one they redefined under the managed name, is
// not, and neither is any server without a managed file.
func TestReadOnlyHintTrusted(t *testing.T) {
	withManaged(t, `{"mcp":{"servers":[{"name":"docs","url":"https://docs.corp.example/mcp","enabled":true,"trust_read_only_hint":true},{"name":"wiki","url":"https://wiki.corp.example/mcp","enabled":true}]}}`)
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"),
		[]byte(`{"mcp":{"servers":[{"name":"mine","url":"https://mine.example/mcp","enabled":true}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range cfg.MCP.Servers {
		got[s.Name] = cfg.ReadOnlyHintTrusted(s)
	}
	if !got["docs"] {
		t.Errorf("the managed file's opted-in server is not believed: %v %+v", got, cfg.MCP.Servers)
	}
	// A managed server that does not opt in is not believed: upgrading
	// changes nothing.
	if v, ok := got["wiki"]; !ok || v {
		t.Errorf("a managed server without trust_read_only_hint was believed: %v", got)
	}
	if v, ok := got["mine"]; ok && v {
		t.Error("a person's own server counted as the operator's")
	}
	// The same name with another address is not the operator's server.
	if cfg.ReadOnlyHintTrusted(MCPServerConfig{Name: "docs", URL: "https://evil.example/mcp", Enabled: true, TrustReadOnlyHint: true}) {
		t.Error("a redefined server counted as the operator's")
	}
	noManagedHere := Config{}
	if noManagedHere.ReadOnlyHintTrusted(MCPServerConfig{Name: "docs", URL: "https://docs.corp.example/mcp", Enabled: true, TrustReadOnlyHint: true}) {
		t.Error("a server counted as the operator's with no managed file")
	}
}

// A trust_read_only_hint a person sets is ignored, and said so.
func TestReadOnlyHintOutsideTheManagedFileWarns(t *testing.T) {
	withManaged(t, `{"permissions":{"deny":["bash(curl*)"]}}`)
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"),
		[]byte(`{"mcp":{"servers":[{"name":"mine","url":"https://mine.example/mcp","enabled":true,"trust_read_only_hint":true}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var said []string
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Warn: func(s string) { said = append(said, s) }})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range said {
		if strings.Contains(s, "MCP server mine sets trust_read_only_hint, which is ignored") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("warnings %q", said)
	}
	for _, s := range cfg.MCP.Servers {
		if cfg.ReadOnlyHintTrusted(s) {
			t.Fatal("a person's opt-in was believed")
		}
	}
}
