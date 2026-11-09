package model

import (
	"math"
	"strings"
)

// Price is what a model costs, in US dollars per million tokens. A zero
// CachedInput or CacheWrite bills those tokens at the Input price.
type Price struct {
	Input       float64 `json:"input"`
	Output      float64 `json:"output"`
	CachedInput float64 `json:"cached_input,omitempty"`
	CacheWrite  float64 `json:"cache_write,omitempty"`
}

// Valid reports a price that can bill: no negative or non-finite figure.
func (p Price) Valid() bool {
	for _, v := range []float64{p.Input, p.Output, p.CachedInput, p.CacheWrite} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// Cost is what u cost at p, in US dollars.
func (p Price) Cost(u Usage) float64 {
	cached, write := p.CachedInput, p.CacheWrite
	if cached == 0 {
		cached = p.Input
	}
	if write == 0 {
		write = p.Input
	}
	plain := u.InputTokens
	if !u.CacheExcluded {
		plain -= u.CachedInputTokens + u.CacheWriteTokens
	}
	plain = max(plain, 0)
	return (float64(plain)*p.Input + float64(u.CachedInputTokens)*cached +
		float64(u.CacheWriteTokens)*write + float64(u.OutputTokens)*p.Output) / 1e6
}

// PricesAsOf is when the built-in prices were taken from the providers'
// published price lists. Prices change; a configured price overrides these.
const PricesAsOf = "2026-11"

// builtinPrices are list prices for models whose price is published per
// token and does not depend on the prompt's length. A model not named here
// is unpriced: its cost is not shown and is never guessed.
var builtinPrices = map[string]Price{
	"gpt-4o":            {Input: 2.50, Output: 10.00, CachedInput: 1.25},
	"gpt-4o-2024-08-06": {Input: 2.50, Output: 10.00, CachedInput: 1.25},
	"gpt-4o-2024-11-20": {Input: 2.50, Output: 10.00, CachedInput: 1.25},
	"gpt-4o-mini":       {Input: 0.15, Output: 0.60, CachedInput: 0.075},
	"gpt-4.1":           {Input: 2.00, Output: 8.00, CachedInput: 0.50},
	"gpt-4.1-mini":      {Input: 0.40, Output: 1.60, CachedInput: 0.10},
	"gpt-4.1-nano":      {Input: 0.10, Output: 0.40, CachedInput: 0.025},
	"gpt-5":             {Input: 1.25, Output: 10.00, CachedInput: 0.125},
	"gpt-5-mini":        {Input: 0.25, Output: 2.00, CachedInput: 0.025},
	"gpt-5-nano":        {Input: 0.05, Output: 0.40, CachedInput: 0.005},
	"o3":                {Input: 2.00, Output: 8.00, CachedInput: 0.50},
	"o4-mini":           {Input: 1.10, Output: 4.40, CachedInput: 0.275},

	"claude-sonnet-4-5":          {Input: 3.00, Output: 15.00, CachedInput: 0.30, CacheWrite: 3.75},
	"claude-sonnet-4-5-20250929": {Input: 3.00, Output: 15.00, CachedInput: 0.30, CacheWrite: 3.75},
	"claude-sonnet-4-20250514":   {Input: 3.00, Output: 15.00, CachedInput: 0.30, CacheWrite: 3.75},
	"claude-opus-4-1":            {Input: 15.00, Output: 75.00, CachedInput: 1.50, CacheWrite: 18.75},
	"claude-opus-4-1-20250805":   {Input: 15.00, Output: 75.00, CachedInput: 1.50, CacheWrite: 18.75},
	"claude-opus-4-20250514":     {Input: 15.00, Output: 75.00, CachedInput: 1.50, CacheWrite: 18.75},
	"claude-haiku-4-5":           {Input: 1.00, Output: 5.00, CachedInput: 0.10, CacheWrite: 1.25},
	"claude-haiku-4-5-20251001":  {Input: 1.00, Output: 5.00, CachedInput: 0.10, CacheWrite: 1.25},
	"claude-3-5-haiku-20241022":  {Input: 0.80, Output: 4.00, CachedInput: 0.08, CacheWrite: 1.00},
}

// PriceFor is the price of a model by its name as sent to the provider:
// a configured price first, then the built-in list. ok is false for a model
// with no price, which is then shown as unpriced.
func PriceFor(model string, configured map[string]Price) (Price, bool) {
	if p, ok := configured[model]; ok {
		return p, p.Valid()
	}
	p, ok := builtinPrices[strings.ToLower(model)]
	return p, ok
}
