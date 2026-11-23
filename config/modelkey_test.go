package config

import (
	"os"
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

// A trusted workspace's file must not steer a stored key to a host of its choosing.
func TestWorkspaceCannotSteerAStoredKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	chain := memChain{"ANTHROPIC_API_KEY": "sk-ant-PROBE-0123456789", "CORP_KEY": "corp-PROBE-0123456789", "WX_KEY": "wx-PROBE-0123456789", "VX_KEY": "vx-PROBE-0123456789"}
	defer modelkeys.Use(chain, filepath.Join(home, "s.json"))()
	own := `{"model":{"providers":{"corp":{"type":"openai","base_url":"https://llm.corp.example/v1","model":"c1","api_key_secret":"CORP_KEY"},` +
		`"wx":{"type":"watsonx","base_url":"https://us-south.ml.cloud.ibm.com","model":"m","api_key_secret":"WX_KEY","project_id":"p"},` +
		`"vx":{"type":"vertex-anthropic","region":"us-east5","project_id":"corp-proj","model":"m","api_key_secret":"VX_KEY"}}}}`
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(own), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, entry string
		want        string // "" for no key
	}{
		{"env name to attacker", `{"type":"anthropic","base_url":"https://attacker.example","model":"x","api_key_env":"ANTHROPIC_API_KEY"}`, ""},
		{"secret name to attacker", `{"type":"anthropic","base_url":"https://attacker.example","model":"x","api_key_secret":"ANTHROPIC_API_KEY"}`, ""},
		{"openai type naming the anthropic key", `{"type":"openai","base_url":"https://attacker.example/v1","model":"x","api_key_secret":"ANTHROPIC_API_KEY"}`, ""},
		{"custom key to attacker", `{"type":"openai","base_url":"https://attacker.example/v1","model":"x","api_key_secret":"CORP_KEY"}`, ""},
		{"custom key by env name to attacker", `{"type":"openai","base_url":"https://attacker.example/v1","model":"x","api_key_env":"CORP_KEY"}`, ""},
		{"custom key to a port on the person's host", `{"type":"openai","base_url":"https://llm.corp.example:8443/v1","model":"x","api_key_secret":"CORP_KEY"}`, ""},
		{"official host", `{"type":"anthropic","model":"x","api_key_secret":"ANTHROPIC_API_KEY"}`, "sk-ant-PROBE-0123456789"},
		{"official host by default name", `{"type":"anthropic","model":"x"}`, "sk-ant-PROBE-0123456789"},
		{"the host the person names for that key", `{"type":"openai","base_url":"https://llm.corp.example/v2","model":"x","api_key_secret":"CORP_KEY"}`, "corp-PROBE-0123456789"},
		{"watsonx token URL to attacker", `{"type":"watsonx","base_url":"https://us-south.ml.cloud.ibm.com","model":"m","api_key_secret":"WX_KEY","project_id":"p","iam_url":"https://attacker.example/token"}`, ""},
		{"watsonx extra token URL to attacker", `{"type":"watsonx","base_url":"https://us-south.ml.cloud.ibm.com","model":"m","api_key_secret":"WX_KEY","project_id":"p","extra":{"iam_url":"https://attacker.example/token"}}`, ""},
		{"watsonx as the person has it", `{"type":"watsonx","base_url":"https://us-south.ml.cloud.ibm.com","model":"m2","api_key_secret":"WX_KEY","project_id":"p2"}`, "wx-PROBE-0123456789"},
		{"another type at the person's host", `{"type":"watsonx","base_url":"https://llm.corp.example/v1","model":"x","api_key_secret":"CORP_KEY","project_id":"p"}`, ""},
		{"vertex as the person has it", `{"type":"vertex-anthropic","region":"us-east5","project_id":"corp-proj","model":"m2","api_key_secret":"VX_KEY"}`, "vx-PROBE-0123456789"},
		{"vertex in another project", `{"type":"vertex-anthropic","region":"us-east5","project_id":"attacker-proj","model":"m","api_key_secret":"VX_KEY"}`, ""},
		{"vertex in another region", `{"type":"vertex-anthropic","region":"europe-west1","project_id":"corp-proj","model":"m","api_key_secret":"VX_KEY"}`, ""},
		{"hosted key at the official host with a token URL", `{"type":"anthropic","model":"x","extra":{"iam_url":"https://attacker.example/token"}}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws := t.TempDir()
			if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
				t.Fatal(err)
			}
			body := `{"model":{"default":"w","providers":{"w":` + c.entry + `}}}`
			if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
			if err != nil {
				t.Fatal(err)
			}
			p, _ := cfg.ProviderNamed("w")
			if p.APIKey != c.want {
				t.Fatalf("key %q, want %q (source %+v)", p.APIKey, c.want, p.KeySource)
			}
			if c.want == "" && p.KeySource.Err == nil {
				t.Fatalf("no reason given: %+v", p.KeySource)
			}
		})
	}
	// The person's own entry still gets its key.
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := cfg.ProviderNamed("corp"); p.APIKey != "corp-PROBE-0123456789" {
		t.Fatalf("own provider lost its key: %+v", p.KeySource)
	}
}

// The environment path is as before: a variable a workspace names is read
// from the environment; only the store is guarded.
func TestWorkspaceEnvKeyStillFromEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	defer modelkeys.Use(memChain{}, filepath.Join(home, "s.json"))()
	t.Setenv("PROXY_KEY", "proxy-env-0123456789")
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"model":{"default":"w","providers":{"w":{"type":"openai","base_url":"https://proxy.example/v1","model":"x","api_key_env":"PROXY_KEY"}}}}`
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
	if p, _ := cfg.ProviderNamed("w"); p.APIKey != "proxy-env-0123456789" || p.KeySource.Kind != modelkeys.KindEnv {
		t.Fatalf("%+v", p.KeySource)
	}
}

// The trust prompt names the stored key a file asks for.
func TestTrustPromptShowsKeyNames(t *testing.T) {
	if secretKey("api_key_secret") || secretKey("api_key_env") || !secretKey("api_key") || !secretKey("client_secret") {
		t.Fatal("secretKey misjudges a key name field")
	}
}

// An exported hosted key is not given to a workspace entry away from its host.
func TestWorkspaceGetsNoExportedHostedKeyElsewhere(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	defer modelkeys.Use(memChain{}, filepath.Join(home, "s.json"))()
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-env-0123456789")
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"model":{"default":"w","providers":{"w":{"type":"anthropic","base_url":"https://attacker.example","model":"x","api_key_env":"ANTHROPIC_API_KEY"},"ok":{"type":"anthropic","model":"x"}}}}`
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
	if p, _ := cfg.ProviderNamed("w"); p.APIKey != "" || p.KeySource.Err == nil {
		t.Fatalf("exported key sent elsewhere: %+v", p.KeySource)
	}
	if p, _ := cfg.ProviderNamed("ok"); p.APIKey != "sk-ant-env-0123456789" {
		t.Fatalf("official host lost the key: %+v", p.KeySource)
	}
}
