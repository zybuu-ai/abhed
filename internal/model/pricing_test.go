package model

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Cached tokens are part of the prompt for OpenAI-shaped usage and apart from
// it for Anthropic's; each is billed at its own price, never twice.
func TestPriceCost(t *testing.T) {
	p := Price{Input: 2.50, Output: 10.00, CachedInput: 1.25}
	// 1,000 prompt tokens of which 400 cached, 200 out.
	got := p.Cost(Usage{InputTokens: 1000, CachedInputTokens: 400, OutputTokens: 200})
	if want := (600*2.50 + 400*1.25 + 200*10.00) / 1e6; !near(got, want) {
		t.Fatalf("openai-shaped cost %v, want %v", got, want)
	}
	a := Price{Input: 3, Output: 15, CachedInput: 0.30, CacheWrite: 3.75}
	got = a.Cost(Usage{InputTokens: 100, CachedInputTokens: 900, CacheWriteTokens: 500, OutputTokens: 50, CacheExcluded: true})
	if want := (100*3 + 900*0.30 + 500*3.75 + 50*15) / 1e6; !near(got, want) {
		t.Fatalf("anthropic-shaped cost %v, want %v", got, want)
	}
	// No cached price: cached tokens at the input price.
	if got := (Price{Input: 1, Output: 2}).Cost(Usage{InputTokens: 10, CachedInputTokens: 10}); !near(got, 10.0/1e6) {
		t.Fatalf("cost %v", got)
	}
}

// A model not in the list or the configuration has no price, and a broken
// configured price is no price either: neither is ever guessed.
func TestPriceFor(t *testing.T) {
	if _, ok := PriceFor("gpt-4o", nil); !ok {
		t.Fatal("gpt-4o is in the list")
	}
	if _, ok := PriceFor("GPT-4o", nil); !ok {
		t.Fatal("the list is matched without case")
	}
	// Unconfirmed, retired, or priced by prompt length: unpriced.
	for _, name := range []string{"gemma4:26b", "gpt-4o-2024-05-13", "my-finetune", "",
		"gpt-4o-2024-08-06", "gpt-4o-2024-11-20", "gpt-4.1-nano", "o4-mini", "claude-3-5-haiku-20241022",
		"claude-opus-4-1", "claude-sonnet-4-20250514", "claude-haiku-5-5", "gpt-6-astra", "gpt-6-luna", "gpt-5.5"} {
		if _, ok := PriceFor(name, nil); ok {
			t.Errorf("%q has no price and must not be guessed", name)
		}
	}
	if p, ok := PriceFor("claude-opus-5-5", nil); !ok || p != (Price{Input: 4, Output: 20, CachedInput: 0.20, CacheWrite: 5}) {
		t.Fatalf("claude-opus-5-5 %+v %v", p, ok)
	}
	if p, ok := PriceFor("gpt-5.4-mini", nil); !ok || p != (Price{Input: 0.75, Output: 4.50, CachedInput: 0.075}) {
		t.Fatalf("gpt-5.4-mini %+v %v", p, ok)
	}
	conf := map[string]Price{"gemma4:26b": {Input: 0, Output: 0}, "gpt-4o": {Input: 9, Output: 9}, "bad": {Input: -1, Output: 1}}
	if p, ok := PriceFor("gemma4:26b", conf); !ok || p.Input != 0 {
		t.Fatal("a configured zero price is a price")
	}
	if p, _ := PriceFor("gpt-4o", conf); p.Input != 9 {
		t.Fatal("a configured price overrides the list")
	}
	if _, ok := PriceFor("bad", conf); ok {
		t.Fatal("a negative configured price is no price")
	}
}
