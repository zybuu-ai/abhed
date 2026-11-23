package config

import (
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/modelkeys"
)

type memChain map[string]string

func (m memChain) Get(n string) (string, bool, error) { v, ok := m[n]; return v, ok, nil }
func (m memChain) Set(n, v string) error              { m[n] = v; return nil }
func (m memChain) Delete(n string) (bool, error)      { _, ok := m[n]; delete(m, n); return ok, nil }

func TestResolveKeySources(t *testing.T) {
	chain := memChain{"ANTHROPIC_API_KEY": "sk-stored-0123456789", "CORP_KEY": "corp-stored-0123456789"}
	defer modelkeys.Use(chain, filepath.Join(t.TempDir(), "s.json"))()

	// Nothing named: the type's own name, from the Keychain.
	p := ResolveKey(ProviderConfig{Type: "anthropic", Model: "claude-sonnet-5-5"})
	if p.APIKey != "sk-stored-0123456789" || p.KeySource.Kind != modelkeys.KindKeychain || p.KeySource.Name != "ANTHROPIC_API_KEY" {
		t.Fatalf("stored: %+v", p.KeySource)
	}
	// The environment wins when set.
	t.Setenv("ANTHROPIC_API_KEY", "sk-env-0123456789")
	p = ResolveKey(ProviderConfig{Type: "anthropic", Model: "claude-sonnet-5-5"})
	if p.APIKey != "sk-env-0123456789" || p.KeySource.Kind != modelkeys.KindEnv {
		t.Fatalf("env: %+v", p.KeySource)
	}
	// Another endpoint is never sent the type's key.
	p = ResolveKey(ProviderConfig{Type: "anthropic", BaseURL: "https://proxy.example.com", Model: "m"})
	if p.APIKey != "" {
		t.Fatal("a proxy was given the Anthropic key")
	}
	// api_key_secret names a stored key; api_key_env still wins over it.
	p = ResolveKey(ProviderConfig{Type: "openai", BaseURL: "https://corp.example.com/v1", APIKeySecret: "CORP_KEY"})
	if p.APIKey != "corp-stored-0123456789" || p.KeySource.Kind != modelkeys.KindKeychain {
		t.Fatalf("secret: %+v", p.KeySource)
	}
	t.Setenv("CORP_ENV", "corp-env-0123456789")
	p = ResolveKey(ProviderConfig{Type: "openai", BaseURL: "https://corp.example.com/v1", APIKeySecret: "CORP_KEY", APIKeyEnv: "CORP_ENV"})
	if p.APIKey != "corp-env-0123456789" || p.KeySource.Kind != modelkeys.KindEnv {
		t.Fatalf("env over secret: %+v", p.KeySource)
	}
	// None anywhere: the name it goes by, no key.
	p = ResolveKey(ProviderConfig{Type: "groq", Model: "m"})
	if p.APIKey != "" || p.KeySource.Kind != "" || p.KeySource.Name != "GROQ_API_KEY" {
		t.Fatalf("none: %+v", p.KeySource)
	}
}

func TestProviderNamedUsesStoredKey(t *testing.T) {
	defer modelkeys.Use(memChain{"ANTHROPIC_API_KEY": "sk-stored-0123456789"}, filepath.Join(t.TempDir(), "s.json"))()
	c := Config{Model: ModelConfig{Default: "c", Providers: map[string]ProviderConfig{"c": {Type: "anthropic", Model: "claude-opus-5-5"}}}}
	p, err := c.ProviderNamed("c")
	if err != nil || p.APIKey != "sk-stored-0123456789" {
		t.Fatalf("ProviderNamed = %q, %v", p.APIKey, err)
	}
	if c.Model.Providers["c"].APIKey != "" {
		t.Fatal("the key was written back into the configuration")
	}
}
