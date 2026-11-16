package model

import (
	"maps"
	"math"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
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

// PricesAsOf is the day the built-in prices were checked, Standard tier, on
// developers.openai.com/api/docs/pricing and platform.claude.com/docs/en/about-claude/pricing.
const PricesAsOf = "2026-11-09"

// builtinPrices are list prices for models whose price is published per
// token and does not depend on the prompt's length; models priced by prompt
// length (gpt-6-*, gpt-5.4, gpt-5.5, Claude Haiku 5.5) and retired ones are
// left out. A model not named here is unpriced, never guessed. Claude cache
// writes are the 5-minute price, the cache Abhed asks for.
var builtinPrices = map[string]Price{
	"gpt-4o":       {Input: 2.50, Output: 10.00, CachedInput: 1.25},
	"gpt-4o-mini":  {Input: 0.15, Output: 0.60, CachedInput: 0.075},
	"gpt-4.1":      {Input: 2.00, Output: 8.00, CachedInput: 0.50},
	"gpt-4.1-mini": {Input: 0.40, Output: 1.60, CachedInput: 0.10},
	"gpt-5":        {Input: 1.25, Output: 10.00, CachedInput: 0.125},
	"gpt-5-mini":   {Input: 0.25, Output: 2.00, CachedInput: 0.025},
	"gpt-5-nano":   {Input: 0.05, Output: 0.40, CachedInput: 0.005},
	"gpt-5.4-mini": {Input: 0.75, Output: 4.50, CachedInput: 0.075},
	"o3":           {Input: 2.00, Output: 8.00, CachedInput: 0.50},

	"claude-opus-5-5":            {Input: 4.00, Output: 20.00, CachedInput: 0.20, CacheWrite: 5.00},
	"claude-sonnet-5-5":          {Input: 2.00, Output: 10.00, CachedInput: 0.10, CacheWrite: 2.50},
	"claude-sonnet-4-5":          {Input: 3.00, Output: 15.00, CachedInput: 0.30, CacheWrite: 3.75},
	"claude-sonnet-4-5-20250929": {Input: 3.00, Output: 15.00, CachedInput: 0.30, CacheWrite: 3.75},
	"claude-haiku-4-5":           {Input: 1.00, Output: 5.00, CachedInput: 0.10, CacheWrite: 1.25},
	"claude-haiku-4-5-20251001":  {Input: 1.00, Output: 5.00, CachedInput: 0.10, CacheWrite: 1.25},
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

// knownHosted are current hosted models with no confirmed list price here:
// unpriced, so a dollar cap refuses them until an operator prices them, and
// named, so no org's 0 can price them either.
var knownHosted = []string{
	"gpt-3.5-turbo", "gpt-4", "gpt-4-turbo", "gpt-4.1-nano", "gpt-4.5", "gpt-4.5-preview", "gpt-5.1",
	"gpt-5-codex", "gpt-5-pro", "chatgpt-4o-latest", "o1", "o1-mini", "o1-pro", "o3-mini", "o3-pro", "o4-mini",
	"claude-opus-4", "claude-opus-4-1", "claude-opus-4-5", "claude-opus-4-6", "claude-sonnet-4", "claude-sonnet-4-6",
	"claude-3-5-sonnet", "claude-3-5-haiku", "claude-3-7-sonnet", "claude-3-opus", "claude-3-haiku",
	"gemini-1.5-pro", "gemini-1.5-flash", "gemini-2.0-flash", "gemini-2.5-pro", "gemini-2.5-flash",
	"grok-4", "mistral-large", "command-r-plus",
}

// unicodeDashes are the hyphens and dashes taken as "-" in a model name.
const unicodeDashes = "\u2010\u2011\u2012\u2013\u2014\u2015\u2212\ufe58\ufe63\uff0d"

// NormalModelName is a model name as the price lists compare it: NFKC,
// format characters (zero-width and the like) removed, Unicode dashes as
// "-", lower case, and a provider/ prefix dropped. ok is false for a name
// that changed under trimming or format removal, which is ambiguous.
func NormalModelName(name string) (n string, ok bool) {
	n = norm.NFKC.String(name)
	stripped := strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Cf, r):
			return -1
		case strings.ContainsRune(unicodeDashes, r):
			return '-'
		}
		return r
	}, n)
	ok = stripped == n
	n = strings.TrimSpace(stripped)
	ok = ok && n == strings.TrimSpace(name) && n == name && n != ""
	n = strings.ToLower(n)
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	return n, ok
}

// ListedOrAlias reports a model name that is, or may stand for, a model on
// the built-in list or a known hosted model: once normalised
// (NormalModelName), a listed name itself or one of its dated or tagged
// variants (gpt-4o-2024-08-06, openai/gpt-4o, claude-sonnet-4-5@20250929).
// A name that normalising changed beyond case and prefix is ambiguous and
// counts as listed. Short keys match widely (o3 takes every o3-* name), so
// some local models named after a listed family count too: failing closed.
func ListedOrAlias(name string) bool {
	n, ok := NormalModelName(name)
	if !ok {
		return true
	}
	keys := slices.Collect(maps.Keys(builtinPrices))
	for _, k := range append(keys, knownHosted...) {
		if n == k {
			return true
		}
		for _, sep := range []string{"-", ":", "@", "_", "."} {
			if strings.HasPrefix(n, k+sep) {
				return true
			}
		}
	}
	return false
}
