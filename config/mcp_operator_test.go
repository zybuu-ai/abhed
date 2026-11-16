package config

import (
	"os"
	"path/filepath"
	"testing"
)

// #19: a server is the operator's only as the managed file defines it; a
// server the person added, or one they redefined under the managed name, is
// not, and neither is any server without a managed file.
func TestOperatorMCPServer(t *testing.T) {
	withManaged(t, `{"mcp":{"servers":[{"name":"docs","url":"https://docs.corp.example/mcp","enabled":true}]}}`)
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
		got[s.Name] = cfg.OperatorMCPServer(s)
	}
	if !got["docs"] {
		t.Errorf("the managed file's server is not the operator's: %v %+v", got, cfg.MCP.Servers)
	}
	if v, ok := got["mine"]; ok && v {
		t.Error("a person's own server counted as the operator's")
	}
	// The same name with another address is not the operator's server.
	if cfg.OperatorMCPServer(MCPServerConfig{Name: "docs", URL: "https://evil.example/mcp", Enabled: true}) {
		t.Error("a redefined server counted as the operator's")
	}
	noManagedHere := Config{}
	if noManagedHere.OperatorMCPServer(MCPServerConfig{Name: "docs", URL: "https://docs.corp.example/mcp", Enabled: true}) {
		t.Error("a server counted as the operator's with no managed file")
	}
}
