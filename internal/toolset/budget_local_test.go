package toolset

import (
	"context"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// An unpinned org's 0 price holds only for a model every provider of which
// runs on this machine; served from anywhere else, it stays unpriced, so a
// dollar budget refuses it rather than count it free.
func TestOrgZeroPriceHoldsOnlyOnThisMachine(t *testing.T) {
	cfg := config.Default()
	cfg.Limits.MaxBudgetUSD = 5
	cfg.Pricing = map[string]config.PriceConfig{"gemma4:26b": {}, "qwen3:8b": {}, "own": {}}
	cfg.PricingLoopbackOnly = map[string]bool{"gemma4:26b": true, "qwen3:8b": true, "llama": true}
	cfg.Model.Providers = map[string]config.ProviderConfig{
		"local":  {Type: "ollama", Model: "gemma4:26b"},
		"vllm":   {Type: "vllm", BaseURL: "http://127.0.0.1:8000/v1", Model: "gemma4:26b"},
		"lan":    {Type: "openai-compatible", BaseURL: "http://10.0.0.5:11434/v1", Model: "qwen3:8b"},
		"ollama": {Type: "ollama", Model: "qwen3:8b"},
		"mine":   {Type: "openai-compatible", BaseURL: "https://api.example.com/v1", Model: "own"},
	}
	b := Budget(cfg)
	if _, ok := b.Price("gemma4:26b"); !ok {
		t.Error("a model served only on this machine was not priced at the org's 0")
	}
	if _, ok := b.Price("qwen3:8b"); ok {
		t.Error("a model one provider sends off this machine kept the org's 0")
	}
	if p, ok := b.Price("own"); !ok || p.Input != 0 {
		t.Error("a price the org did not set was held to this machine")
	}
	if err := b.Refuse("qwen3:8b"); err == nil {
		t.Error("an unpriced model ran under the dollar budget")
	}
}

// A budget the org policy says is spent stops the run at its first turn's
// check, before any model call, and refuses every other call at once.
func TestSpentBudgetStopsBeforeAnyCall(t *testing.T) {
	cfg := config.Default()
	cfg.Limits.BudgetSpent = true
	b := Budget(cfg)
	if err := b.Refuse("gpt-4o"); err == nil {
		t.Fatal("a call went out through a spent budget")
	}
	if a := b.Allowance(context.Background()); !a.Stop || a.Message == "" {
		t.Fatalf("allowance %+v", a)
	}
}

// A loopback address is no proof of a local model: a gateway or signing
// proxy may front a hosted one. An unpinned org's 0 never prices a hosted
// API's type, wherever it sends, and never replaces a built-in list price,
// so gpt-4o and claude stay at list price behind a loopback gateway.
func TestOrgZeroNeverPricesAHostedModel(t *testing.T) {
	for _, c := range []struct {
		name, typ, base, model string
		wantListed             bool // the built-in list price stands
	}{
		{"openai gateway", "openai", "http://127.0.0.1:4000/v1", "gpt-4o", true},
		{"anthropic proxy", "anthropic", "http://localhost:4000", "claude-sonnet-4-5", true},
		{"bedrock signing proxy", "bedrock-anthropic", "http://127.0.0.1:8080", "claude-sonnet-4-5-20250929", true},
		{"openai-compatible gateway, listed name", "openai-compatible", "http://127.0.0.1:4000/v1", "gpt-4o", true},
		{"openai type, unlisted name", "openai", "http://127.0.0.1:4000/v1", "house-model", false},
		{"custom type", "acme-gw", "http://127.0.0.1:4000/v1", "house-model", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Limits.MaxBudgetUSD = 1
			cfg.Pricing = map[string]config.PriceConfig{c.model: {}}
			cfg.PricingLoopbackOnly = map[string]bool{c.model: true}
			cfg.Model.Providers = map[string]config.ProviderConfig{"gw": {Type: c.typ, BaseURL: c.base, Model: c.model}}
			b := Budget(cfg)
			p, ok := b.Price(c.model)
			if c.wantListed {
				if !ok || p.Input == 0 {
					t.Fatalf("the list price was replaced: %+v %v", p, ok)
				}
				return
			}
			if ok {
				t.Fatalf("priced at %+v", p)
			}
			if b.Refuse(c.model) == nil {
				t.Fatal("an unpriced hosted model ran under the dollar budget")
			}
		})
	}
}

// Only the local model server types count, and never for a name that may
// stand for a listed model: an openai-compatible endpoint on loopback is
// the shape of a gateway holding a paid key, so its names stay unpriced;
// a dated variant, a provider/ prefix or surrounding spaces of a listed name
// is never 0, whoever serves it.
func TestOrgZeroAliasesAndGateways(t *testing.T) {
	for _, c := range []struct {
		typ, base, model string
		zero             bool
	}{
		{"openai-compatible", "http://127.0.0.1:4000/v1", "gpt-4o-2024-08-06", false},
		{"openai-compatible", "http://127.0.0.1:4000/v1", "openai/gpt-4o", false},
		{"openai-compatible", "http://127.0.0.1:4000/v1", "gpt-4o ", false},
		{"openai-compatible", "http://127.0.0.1:4000/v1", "claude-sonnet-4-6", false},
		{"openai-compatible", "http://127.0.0.1:4000/v1", "my-gpt", false},
		{"ollama", "", "gpt-4o-2024-08-06", false},
		{"ollama", "", "openai/gpt-4o", false},
		{"vllm", "http://127.0.0.1:8000/v1", "Claude-Sonnet-4-5@20250929", false},
		{"ollama", "", "gemma4:26b", true},
		{"llamacpp", "http://localhost:8080/v1", "my-gpt", true},
		{"ollama", "", "gpt-oss:120b-cloud", true}, // billed to the person's own Ollama account
	} {
		cfg := config.Default()
		cfg.Limits.MaxBudgetUSD = 1
		cfg.Pricing = map[string]config.PriceConfig{c.model: {}}
		cfg.PricingLoopbackOnly = map[string]bool{c.model: true}
		cfg.Model.Providers = map[string]config.ProviderConfig{"p": {Type: c.typ, BaseURL: c.base, Model: c.model}}
		b := Budget(cfg)
		p, ok := b.Price(c.model)
		zero := ok && p.Input == 0 && p.Output == 0
		if zero != c.zero {
			t.Errorf("%s %q: zero %v (price %+v %v), want %v", c.typ, c.model, zero, p, ok, c.zero)
		}
	}
}
