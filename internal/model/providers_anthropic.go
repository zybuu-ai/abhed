package model

import (
	"fmt"
	"os"
)

func init() {
	Register("anthropic", "Anthropic Messages API (api.anthropic.com)", anthropicStyle(""))
}

// anthropicStyle builds the factory for an Anthropic-compatible endpoint, so a
// custom provider from configuration reuses exactly the built-in path.
func anthropicStyle(defaultURL string) Factory {
	return func(s Spec) (Adapter, error) {
		if s.BaseURL == "" {
			s.BaseURL = defaultURL
		}
		return newAnthropicAdapter(s)
	}
}

func newAnthropicAdapter(s Spec) (Adapter, error) {
	{
		if s.Model == "" {
			return nil, fmt.Errorf("provider %q needs a model", s.Type)
		}
		a := NewAnthropic(s.BaseURL, s.APIKey, s.Model, Profile{
			Name:            s.Model,
			ContextWindow:   s.ContextWindow,
			MaxOutputTokens: s.MaxOutputTokens,
			SupportsTools:   true,
			SupportsVision:  true,
			SupportsStream:  true,
			ToolCallFormat:  "json",
			ReasoningTokens: true,
			// Explicit cache_control on the system block, which is what
			// makes the stable prefix a cache read rather than a re-bill.
			CachePrefix: true,
			Sampling: Sampling{
				Temperature: true, TopP: true, TopK: true, Stop: true,
				Think: true, ThinkingBudget: true, Effort: true,
			},
		})
		a.Defaults = s.Params
		// A subscription token, if the operator supplied one: config first, then
		// Abhed's own variable, then the name an existing setup-token writes.
		a.Bearer = firstNonEmptyString(
			s.Get("oauth_token"),
			os.Getenv("ABHED_ANTHROPIC_OAUTH_TOKEN"),
			os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"),
			os.Getenv("ANTHROPIC_AUTH_TOKEN"),
		)
		if v := s.Get("api_version"); v != "" {
			a.Version = v
		}
		if b := s.Get("beta"); b != "" {
			a.Beta = splitList(b)
		}
		return a, nil
	}
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
