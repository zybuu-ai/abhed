package model

import "testing"

// A model's price is found through the wrappers a gateway or cloud puts on
// its id, and only for exactly that model: a family's window is shared, its
// price is not, and another vendor's dated snapshot keeps its own price.
func TestPriceNormalisation(t *testing.T) {
	opus := Price{Input: 4, Output: 20, CachedInput: 0.20, CacheWrite: 5}
	for _, n := range []string{"claude-opus-5-5", "anthropic/claude-opus-5-5", "claude-opus-5-5@20261101",
		"anthropic.claude-opus-5-5-v1:0", "global.anthropic.claude-opus-5-5", "CLAUDE-OPUS-5-5"} {
		if p, ok := PriceFor(n, nil); !ok || p != opus {
			t.Errorf("%q: %+v %v", n, p, ok)
		}
	}
	// A Bedrock regional endpoint is 10% over the global price.
	regional := Price{Input: 4.4, Output: 22, CachedInput: 0.22, CacheWrite: 5.5}
	for _, n := range []string{"us.anthropic.claude-opus-5-5", "eu.anthropic.claude-opus-5-5-v1:0", "apac.anthropic.claude-opus-5-5"} {
		if p, ok := PriceFor(n, nil); !ok || p != regional {
			t.Errorf("%q: %+v %v, want %+v", n, p, ok, regional)
		}
	}
	if p, _ := PriceFor("us.anthropic.claude-haiku-5-5", nil); p.Long == nil || p.Long.Input != 0.55 || p.Input != 0.11 {
		t.Errorf("regional haiku 5.5 %+v", p)
	}
	for n, want := range map[string]Price{
		"claude-opus-5":              {Input: 5, Output: 25, CachedInput: 0.50, CacheWrite: 6.25},
		"claude-sonnet-5":            {Input: 2, Output: 10, CachedInput: 0.20, CacheWrite: 2.50},
		"claude-sonnet-5-5":          {Input: 2, Output: 10, CachedInput: 0.10, CacheWrite: 2.50},
		"claude-sonnet-4-5-20250929": {Input: 3, Output: 15, CachedInput: 0.30, CacheWrite: 3.75},
		"claude-fable-5-2":           {Input: 8, Output: 40, CachedInput: 0.20, CacheWrite: 10},
	} {
		if p, ok := PriceFor(n, nil); !ok || p != want {
			t.Errorf("%q: %+v %v, want %+v", n, p, ok, want)
		}
	}
	// Not the model's own id: unpriced, as before.
	for _, n := range []string{"claude-opus-5-5-preview", "claude-opus", "gpt-4o-2024-05-13", "my-claude-opus-5-5",
		"claude-opus-4-1", "anthropic.claude-sonnet-4-20250514-v1:0", "deepseek-v4-pro"} {
		if _, ok := PriceFor(n, nil); ok {
			t.Errorf("%q is priced", n)
		}
	}
	// A wrapped listed model is listed: no org's zero reaches it.
	if !ListedOrAlias("us.anthropic.claude-opus-5-5-v1:0") || !ListedOrAlias("claude-opus-5") {
		t.Fatal("a wrapped or newly priced Claude is not listed")
	}
}

// Claude Haiku 5.5 is billed by each call's prompt length: up to 100,000
// prompt tokens at the base prices, above it at the long prices, with cache
// reads and writes counted toward the threshold.
func TestHaikuLongPromptTier(t *testing.T) {
	p, ok := PriceFor("claude-haiku-5-5", nil)
	if !ok || !p.Valid() {
		t.Fatalf("haiku 5.5: %+v %v", p, ok)
	}
	at := p.Cost(Usage{InputTokens: 100_000, OutputTokens: 1_000})
	if want := (100_000*0.10 + 1_000*0.50) / 1e6; !near(at, want) {
		t.Fatalf("at the boundary: %v, want %v", at, want)
	}
	over := p.Cost(Usage{InputTokens: 100_001, OutputTokens: 1_000})
	if want := (100_001*0.50 + 1_000*2.50) / 1e6; !near(over, want) {
		t.Fatalf("over the boundary: %v, want %v", over, want)
	}
	// Anthropic reports cache reads and writes apart: they count toward the
	// prompt, and are billed at the long tier's cache prices.
	cached := p.Cost(Usage{InputTokens: 10_000, CachedInputTokens: 80_000, CacheWriteTokens: 20_000, OutputTokens: 100, CacheExcluded: true})
	if want := (10_000*0.50 + 80_000*0.05 + 20_000*0.625 + 100*2.50) / 1e6; !near(cached, want) {
		t.Fatalf("cached long prompt: %v, want %v", cached, want)
	}
	under := p.Cost(Usage{InputTokens: 10_000, CachedInputTokens: 80_000, OutputTokens: 100, CacheExcluded: true})
	if want := (10_000*0.10 + 80_000*0.01 + 100*0.50) / 1e6; !near(under, want) {
		t.Fatalf("cached short prompt: %v, want %v", under, want)
	}
	if PromptTokens(Usage{InputTokens: 5, CachedInputTokens: 3, CacheWriteTokens: 2, CacheExcluded: true}) != 10 ||
		PromptTokens(Usage{InputTokens: 5, CachedInputTokens: 3}) != 5 {
		t.Fatal("prompt tokens")
	}
	// A tier that could not bill is no price.
	for _, bad := range []Price{
		{Input: 1, Output: 1, Long: &Price{Input: 2, Output: 2}},
		{Input: 1, Output: 1, LongAbove: 10, Long: &Price{Input: -1, Output: 2}},
		{Input: 1, Output: 1, LongAbove: -1},
	} {
		if bad.Valid() {
			t.Errorf("%+v is valid", bad)
		}
	}
}
