package config

import "testing"

// A workspace file may only turn completions off or keep them local, and
// cannot name the provider the code goes to.
func TestCompletionSettingsOnlyTighten(t *testing.T) {
	_, ws := trustHome(t, `{"model":{"default":"m","completion":"m","providers":{"m":{"type":"ollama","model":"qwen2.5-coder"},"cloud":{"type":"openai","model":"x"}}}}`,
		`{"completion":{"enabled":false,"local_only":true},"model":{"completion":"cloud"}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Completion != "m" || cfg.CompletionsOn() || !cfg.Completion.LocalOnly {
		t.Fatalf("completion %q on %v local %v", cfg.Model.Completion, cfg.CompletionsOn(), cfg.Completion.LocalOnly)
	}
	_, ws = trustHome(t, `{"model":{"default":"m","completion":"m","providers":{"m":{"type":"ollama","model":"q"}}},"completion":{"enabled":false}}`,
		`{"completion":{"enabled":true,"local_only":false}}`)
	cfg, err = LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil || cfg.CompletionsOn() {
		t.Fatalf("a workspace turned completions back on: %v %v", cfg.CompletionsOn(), err)
	}
	c := Default()
	c.Model.Completion = "nope"
	if err := c.Validate(); err == nil {
		t.Fatal("an unknown completion provider was taken")
	}
}

func TestOnThisMachine(t *testing.T) {
	for _, c := range []struct {
		p    ProviderConfig
		want bool
	}{
		{ProviderConfig{Type: "ollama"}, true}, {ProviderConfig{Type: "openai"}, false},
		{ProviderConfig{Type: "openai-compatible", BaseURL: "http://127.0.0.1:11434/v1"}, true},
		{ProviderConfig{Type: "openai-compatible", BaseURL: "http://localhost:8000/v1"}, true},
		{ProviderConfig{Type: "openai-compatible", BaseURL: "http://[::1]:8000/v1"}, true},
		{ProviderConfig{Type: "ollama", BaseURL: "http://gpu-box:11434/v1"}, false},
		{ProviderConfig{Type: "openai-compatible", BaseURL: "https://api.example.com/v1"}, false},
	} {
		if c.p.OnThisMachine() != c.want {
			t.Errorf("%+v: %v", c.p, !c.want)
		}
	}
}
