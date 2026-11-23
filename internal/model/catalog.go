package model

import (
	"regexp"
	"strings"
)

// The catalog is the one table of what Abhed knows about named models: the
// context window, the longest reply, and the list price. The context window
// and output cap are matched by family, the longest prefix of the model id,
// so a dated or tagged id ("claude-haiku-4-5-20251001", "llama3.1:70b")
// finds its row. A price is matched only by the model's own id, once the
// wrappers a gateway or a cloud puts around it are removed (see
// CatalogID): a family's window is shared by every member, its price is not.
//
// Sources: platform.claude.com/docs/en/about-claude/pricing and the Claude
// models overview for Claude; developers.openai.com/api/docs/models and
// /pricing for OpenAI; aistudio.google.com/docs/models for Gemini;
// docs.x.ai/docs/models for Grok; each open-weight family's model card.

// PricesAsOf is the day the built-in prices were last checked, Standard
// tier: the Claude prices on platform.claude.com/docs/en/about-claude/pricing
// on this day, the OpenAI ones on developers.openai.com/api/docs/pricing on
// 2026-11-09.
const PricesAsOf = "2026-11-23"

// catalogRow is one model or family.
type catalogRow struct {
	// id is the model id, or the family prefix every member's id starts with.
	id     string
	window int // context window in tokens; zero leaves it to the next row down
	output int // longest reply in tokens; zero where not published
	// price is the list price of exactly this id, nil for none here.
	price *Price
	// hosted marks a vendor's hosted model, which an org's zero price must
	// never reach (see ListedOrAlias); open-weight families are not.
	hosted bool
}

func usd(in, out, cached, write float64) *Price {
	return &Price{Input: in, Output: out, CachedInput: cached, CacheWrite: write}
}

// claudeHaiku55 is priced by prompt length: a call whose prompt (every input
// token, cache reads and writes included) is over 100,000 tokens pays the
// higher prices, and each call is priced on its own.
var claudeHaiku55 = &Price{Input: 0.10, Output: 0.50, CachedInput: 0.01, CacheWrite: 0.125,
	LongAbove: 100_000, Long: usd(0.50, 2.50, 0.05, 0.625)}

// catalog rows. Claude cache writes are the 5-minute price, the cache Abhed
// asks for. A model priced by prompt length that has no published tier
// structure here (gpt-6-*, gpt-5.4, gpt-5.5) and retired models are left
// unpriced, never guessed.
var catalog = []catalogRow{
	// Anthropic.
	{id: "claude-fable-5-2", window: 1_000_000, output: 128_000, price: usd(8, 40, 0.20, 10), hosted: true},
	{id: "claude-fable-5-1", window: 1_000_000, output: 128_000, price: usd(10, 50, 0.25, 12.50), hosted: true},
	{id: "claude-fable-5", window: 1_000_000, output: 128_000, price: usd(10, 50, 1, 12.50), hosted: true},
	{id: "claude-mythos-5-2", window: 1_000_000, output: 128_000, price: usd(8, 40, 0.20, 10), hosted: true},
	{id: "claude-mythos-5-1", window: 1_000_000, output: 128_000, price: usd(10, 50, 0.25, 12.50), hosted: true},
	{id: "claude-mythos-5", window: 1_000_000, output: 128_000, price: usd(10, 50, 1, 12.50), hosted: true},
	{id: "claude-opus-5-5", window: 1_000_000, output: 128_000, price: usd(4, 20, 0.20, 5), hosted: true},
	{id: "claude-opus-5", window: 1_000_000, output: 128_000, price: usd(5, 25, 0.50, 6.25), hosted: true},
	{id: "claude-opus-4-8", window: 1_000_000, output: 128_000, price: usd(5, 25, 0.50, 6.25), hosted: true},
	{id: "claude-opus-4-7", window: 1_000_000, output: 128_000, price: usd(5, 25, 0.50, 6.25), hosted: true},
	{id: "claude-opus-4-6", window: 1_000_000, output: 128_000, price: usd(5, 25, 0.50, 6.25), hosted: true},
	{id: "claude-opus-4-5", window: 200_000, output: 64_000, price: usd(5, 25, 0.50, 6.25), hosted: true},
	{id: "claude-opus-4-1", window: 200_000, output: 32_000, hosted: true}, // retired
	{id: "claude-opus-4", window: 200_000, output: 32_000, hosted: true},   // retired
	{id: "claude-sonnet-5-5", window: 1_000_000, output: 128_000, price: usd(2, 10, 0.10, 2.50), hosted: true},
	{id: "claude-sonnet-5", window: 1_000_000, output: 128_000, price: usd(2, 10, 0.20, 2.50), hosted: true},
	{id: "claude-sonnet-4-6", window: 1_000_000, output: 128_000, price: usd(3, 15, 0.30, 3.75), hosted: true},
	{id: "claude-sonnet-4-5", window: 200_000, output: 64_000, price: usd(3, 15, 0.30, 3.75), hosted: true},
	{id: "claude-sonnet-4", window: 200_000, output: 64_000, hosted: true}, // retired
	{id: "claude-haiku-5-5", window: 1_000_000, output: 128_000, price: claudeHaiku55, hosted: true},
	{id: "claude-haiku-5", window: 1_000_000, output: 128_000, hosted: true},
	{id: "claude-haiku-4-5", window: 200_000, output: 64_000, price: usd(1, 5, 0.10, 1.25), hosted: true},
	{id: "claude-3-7-sonnet", window: 200_000, output: 64_000, hosted: true},
	{id: "claude-3-5-sonnet", window: 200_000, output: 8_192, hosted: true},
	{id: "claude-3-5-haiku", window: 200_000, output: 8_192, hosted: true},
	{id: "claude-3-opus", window: 200_000, output: 4_096, hosted: true},
	{id: "claude-3-haiku", window: 200_000, output: 4_096, hosted: true},
	// Any other Claude: every one since the third generation has had at least this.
	{id: "claude-", window: 200_000},

	// OpenAI.
	{id: "gpt-6", window: 1_000_000, output: 128_000, hosted: true},
	{id: "gpt-5", window: 272_000, output: 128_000, price: usd(1.25, 10, 0.125, 0), hosted: true}, // 400k in all, 272k of it input
	{id: "gpt-5-mini", price: usd(0.25, 2, 0.025, 0), hosted: true},
	{id: "gpt-5-nano", price: usd(0.05, 0.40, 0.005, 0), hosted: true},
	{id: "gpt-5.4-mini", price: usd(0.75, 4.50, 0.075, 0), hosted: true},
	{id: "gpt-4.1", window: 1_047_576, output: 32_768, price: usd(2, 8, 0.50, 0), hosted: true},
	{id: "gpt-4.1-mini", price: usd(0.40, 1.60, 0.10, 0), hosted: true},
	{id: "gpt-4o", window: 128_000, output: 16_384, price: usd(2.50, 10, 1.25, 0), hosted: true},
	{id: "gpt-4o-mini", price: usd(0.15, 0.60, 0.075, 0), hosted: true},
	{id: "gpt-4-turbo", window: 128_000, output: 4_096, hosted: true},
	{id: "gpt-4", window: 8_192, output: 8_192, hosted: true},
	{id: "gpt-3.5-turbo", window: 16_385, output: 4_096, hosted: true},
	{id: "o4-mini", window: 200_000, output: 100_000, hosted: true},
	{id: "o3", window: 200_000, output: 100_000, price: usd(2, 8, 0.50, 0), hosted: true},
	{id: "o1", window: 200_000, output: 100_000, hosted: true},
	{id: "gpt-oss", window: 131_072}, // open weights, served anywhere

	// Google.
	{id: "gemini-4", window: 1_000_000, output: 65_536, hosted: true},
	{id: "gemini-3", window: 1_048_576, output: 65_536, hosted: true},
	{id: "gemini-2.5", window: 1_048_576, output: 65_536, hosted: true},
	{id: "gemini-2.0", window: 1_048_576, output: 8_192, hosted: true},
	{id: "gemini-1.5-pro", window: 2_097_152, output: 8_192, hosted: true},
	{id: "gemini-1.5", window: 1_048_576, output: 8_192, hosted: true},
	{id: "gemini-", window: 1_000_000, output: 8_192, hosted: true},

	// xAI.
	{id: "grok-4.20", window: 1_000_000, hosted: true},
	{id: "grok-4.3", window: 1_000_000, hosted: true},
	{id: "grok-4.5", window: 500_000, hosted: true},
	{id: "grok-4.6", window: 500_000, hosted: true},
	{id: "grok-4.7", window: 500_000, hosted: true},
	{id: "grok-build", window: 500_000, hosted: true},
	{id: "grok-4", window: 256_000, hosted: true},
	{id: "grok-3", window: 131_072, hosted: true},

	// Open-weight families, as trained. A server on this machine may run them
	// with a smaller window; see resolveLimits.
	{id: "llama-3.1", window: 131_072}, {id: "llama-3.2", window: 131_072}, {id: "llama-3.3", window: 131_072},
	{id: "llama3.1", window: 131_072}, {id: "llama3.2", window: 131_072}, {id: "llama3.3", window: 131_072},
	{id: "qwen3-coder", window: 262_144}, {id: "qwen3", window: 32_768}, {id: "qwen2.5", window: 32_768},
	{id: "gemma3", window: 131_072}, {id: "gemma-3", window: 131_072},
	{id: "mistral-nemo", window: 131_072},
	{id: "phi4", window: 16_384}, {id: "phi-4", window: 16_384},
}

// knownHosted are current hosted models with no row and no confirmed list
// price here: unpriced, so a dollar cap refuses them until an operator
// prices them, and named, so no org's 0 can price them either.
var knownHosted = []string{
	"gpt-4.1-nano", "gpt-4.5", "gpt-4.5-preview", "gpt-5.1",
	"gpt-5-codex", "gpt-5-pro", "chatgpt-4o-latest", "o1-mini", "o1-pro", "o3-mini", "o3-pro",
	"mistral-large", "command-r-plus",
}

var (
	// bedrockVersion is Bedrock's "-v1:0" on a model id.
	bedrockVersion = regexp.MustCompile(`-v[0-9]+(:[0-9]+)?$`)
	// claudeDate is a Claude snapshot's date, which names the same model and
	// price as the alias it is pinned under.
	claudeDate = regexp.MustCompile(`^(claude-.+)-20[0-9]{6}$`)
)

// CatalogID is a model id as the catalog compares it: NormalModelName (case,
// Unicode dashes, a gateway's "vendor/" prefix), then without a Bedrock
// region and vendor ("us.anthropic."), a Vertex version ("@20250929"), a
// Bedrock version ("-v1:0") and a Claude snapshot date. Another vendor's
// dated snapshot is kept: OpenAI has priced its snapshots apart.
func CatalogID(name string) string {
	n, _ := NormalModelName(name)
	if i := strings.Index(n, "anthropic."); i >= 0 {
		n = n[i+len("anthropic."):]
	}
	if i := strings.IndexByte(n, '@'); i > 0 {
		n = n[:i]
	}
	if strings.HasPrefix(n, "claude-") {
		n = bedrockVersion.ReplaceAllString(n, "")
		n = claudeDate.ReplaceAllString(n, "$1")
	}
	return n
}

// LookupLimits is the built-in window and output cap for a model id, each
// from the longest prefix whose row has one, and whether any row matched.
func LookupLimits(modelID string) (window, output int, ok bool) {
	m := CatalogID(modelID)
	wLen, oLen := -1, -1
	for _, r := range catalog {
		if !strings.HasPrefix(m, r.id) {
			continue
		}
		if r.window > 0 && len(r.id) > wLen {
			window, wLen = r.window, len(r.id)
		}
		if r.output > 0 && len(r.id) > oLen {
			output, oLen = r.output, len(r.id)
		}
	}
	return window, output, wLen >= 0
}

// builtinPrice is the catalog's list price for exactly this model.
func builtinPrice(name string) (Price, bool) {
	id := CatalogID(name)
	for _, r := range catalog {
		if r.id == id && r.price != nil {
			return *r.price, true
		}
	}
	return Price{}, false
}

// listedNames are the names an org's zero price must never reach: every
// hosted row and every known hosted model.
func listedNames() []string {
	out := make([]string, 0, len(catalog)+len(knownHosted))
	for _, r := range catalog {
		if r.hosted {
			out = append(out, r.id)
		}
	}
	return append(out, knownHosted...)
}
