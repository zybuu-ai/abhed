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
		"vllm":   {Type: "openai-compatible", BaseURL: "http://127.0.0.1:8000/v1", Model: "gemma4:26b"},
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
