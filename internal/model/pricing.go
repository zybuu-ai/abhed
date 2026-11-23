package model

import (
	"math"
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
	// LongAbove, when set, is the prompt length in tokens above which a call
	// is billed at Long instead: every input token of the call counts, cache
	// reads and writes included, and each call is priced on its own.
	LongAbove int    `json:"long_above,omitempty"`
	Long      *Price `json:"long,omitempty"`
}

// Valid reports a price that can bill: no negative or non-finite figure.
func (p Price) Valid() bool {
	if p.Long != nil && (p.LongAbove <= 0 || p.Long.Long != nil || !p.Long.Valid()) {
		return false
	}
	if p.LongAbove < 0 {
		return false
	}
	for _, v := range []float64{p.Input, p.Output, p.CachedInput, p.CacheWrite} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// Cost is what u cost at p, in US dollars.
func (p Price) Cost(u Usage) float64 {
	if p.Long != nil && p.LongAbove > 0 && PromptTokens(u) > p.LongAbove {
		return p.Long.Cost(u)
	}
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

// scaled is p with every figure multiplied by f, its long tier's too.
func (p Price) scaled(f float64) Price {
	r := func(v float64) float64 { return math.Round(v*f*1e6) / 1e6 }
	out := Price{Input: r(p.Input), Output: r(p.Output), CachedInput: r(p.CachedInput), CacheWrite: r(p.CacheWrite), LongAbove: p.LongAbove}
	if p.Long != nil {
		l := p.Long.scaled(f)
		out.Long = &l
	}
	return out
}

// PromptTokens is a call's whole prompt: every input token, cached and
// cache-written included, however the provider reported them.
func PromptTokens(u Usage) int {
	if u.CacheExcluded {
		return u.InputTokens + u.CachedInputTokens + u.CacheWriteTokens
	}
	return u.InputTokens
}

// PriceFor is the price of a model by its name as sent to the provider:
// a configured price first, then the built-in list. ok is false for a model
// with no price, which is then shown as unpriced.
func PriceFor(model string, configured map[string]Price) (Price, bool) {
	if p, ok := configured[model]; ok {
		return p, p.Valid()
	}
	return builtinPrice(model)
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
	if id := CatalogID(name); id != n {
		n = id // a Bedrock or Vertex wrapping of a listed model is listed too
	}
	for _, k := range listedNames() {
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
