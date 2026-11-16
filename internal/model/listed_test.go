package model

import "testing"

// The names an unpinned org's 0 must never reach count as listed: the
// list's own names and their variants, the known hosted models with no
// price here, and Unicode tricks (other dashes, zero-width characters,
// full-width letters, padding). Local model names stay unlisted, except
// those named after a listed family, which fail closed.
func TestListedOrAlias(t *testing.T) {
	listed := []string{
		"gpt-4o", "GPT-4O", " gpt-4o", "gpt-4o ", "gpt-4o-2024-08-06", "openai/gpt-4o", "azure/openai/gpt-4o",
		"gpt-5.1", "gpt-5-codex", "claude-sonnet-4-5@20250929", "o3-pro", "o3:latest",
		"gpt-4-o", "chatgpt-4o-latest", "gpt‑4o", "gpt-4‐o", "ｇｐｔ-4o", "gpt-4​o", "gpt-4o​",
		"claude-sonnet-4-6", "claude-opus-4-6", "gpt-4.5", "o1", "o4-mini", "gemini-2.5-pro",
		// Local names after a listed family: refused, failing closed.
		"o3-mini-local", "gpt-5-nano-distill:7b",
	}
	for _, n := range listed {
		if !ListedOrAlias(n) {
			t.Errorf("%q is not listed", n)
		}
	}
	local := []string{
		"gemma4:26b", "qwen3:8b", "llama3.1:8b", "gpt-oss:20b", "gpt-oss:120b", "gpt-oss-20b", "deepseek-r1:14b",
		"mistral-small3.2", "phi4", "hf.co/unsloth/gpt-oss-20b-GGUF", "o3mini", "openhermes", "gpt4all-falcon",
		"claude-like-7b", "granite3.3:8b",
	}
	for _, n := range local {
		if ListedOrAlias(n) {
			t.Errorf("local model %q is listed", n)
		}
	}
	// Known hosted names carry no invented price.
	for _, n := range []string{"claude-sonnet-4-6", "gpt-4.5", "o1", "o4-mini"} {
		if _, ok := PriceFor(n, nil); ok {
			t.Errorf("%q has a price", n)
		}
	}
}
