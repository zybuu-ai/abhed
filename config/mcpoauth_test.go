package config

import (
	"strings"
	"testing"
)

// An oauth block is refused on a program server, and with a client id,
// secret variable or scope that cannot be one.
func TestMCPOAuthValidation(t *testing.T) {
	ok := MCPServerConfig{Name: "corp", URL: "https://mcp.example.com/mcp", OAuth: &MCPOAuthConfig{ClientID: "abhed-cli", ClientSecretEnv: "CORP_SECRET", Scopes: []string{"read", "tickets:write"}}}
	if err := (Config{MCP: MCPConfig{Servers: []MCPServerConfig{ok}}}).validateMCPOAuth(); err != nil {
		t.Fatal(err)
	}
	for want, s := range map[string]MCPServerConfig{
		"runs a program":     {Name: "x", Command: "/bin/echo", OAuth: &MCPOAuthConfig{}},
		"client_id":          {Name: "x", URL: "https://a/b", OAuth: &MCPOAuthConfig{ClientID: "a\nb"}},
		"environment":        {Name: "x", URL: "https://a/b", OAuth: &MCPOAuthConfig{ClientID: "a", ClientSecretEnv: "A-B"}},
		"needs oauth.client": {Name: "x", URL: "https://a/b", OAuth: &MCPOAuthConfig{ClientSecretEnv: "A"}},
		"not a scope":        {Name: "x", URL: "https://a/b", OAuth: &MCPOAuthConfig{Scopes: []string{"a b"}}},
	} {
		err := (Config{MCP: MCPConfig{Servers: []MCPServerConfig{s}}}).Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}
