package model

import "fmt"

// Cloud-gateway providers: the same model families reached through a hyperscaler.
//
// Each is the wire format of the underlying model with a different URL and a
// different way of signing the request. Abhed carries no cloud SDK, which keeps
// the binary fit for an air-gapped site, so the two that need request signing
// (SigV4 for Bedrock, ADC for Vertex) take a token the operator supplies,
// which is what a sidecar or a short-lived credential already provides.
//
// Being explicit about that beats pretending: an adapter that silently cannot
// authenticate is worse than one that says what it needs.
func init() {
	Register("azure-openai", "Azure OpenAI (deployment endpoint)",
		func(s Spec) (Adapter, error) {
			if s.BaseURL == "" {
				return nil, fmt.Errorf("azure-openai needs base_url, " +
					"e.g. https://<resource>.openai.azure.com/openai/deployments/<deployment>")
			}
			if s.Model == "" {
				return nil, fmt.Errorf("azure-openai needs a model (the deployment name)")
			}
			profile := Profile{
				Name:            s.Model,
				ContextWindow:   s.ContextWindow,
				MaxOutputTokens: s.MaxOutputTokens,
				SupportsTools:   true,
				SupportsVision:  visionFromSpec(s),
				SupportsStream:  true,
				ToolCallFormat:  orElse(s.ToolCallFormat, "json"),
				Sampling:        SamplingOpenAI(),
			}
			a := NewOpenAICompatible(s.BaseURL, s.APIKey, s.Model, profile)
			a.Defaults = s.Params
			return a, nil
		})

	Register("bedrock-anthropic", "Claude on Amazon Bedrock (needs a signed endpoint or proxy)",
		func(s Spec) (Adapter, error) {
			if s.BaseURL == "" {
				return nil, fmt.Errorf("bedrock-anthropic needs base_url — either a " +
					"SigV4-signing proxy, or the Bedrock endpoint when the caller " +
					"supplies credentials through api_key")
			}
			if s.Model == "" {
				return nil, fmt.Errorf("bedrock-anthropic needs a model, " +
					"e.g. anthropic.claude-opus-5")
			}
			a := NewAnthropic(s.BaseURL, s.APIKey, s.Model, anthropicProfile(s))
			a.Defaults = s.Params
			return a, nil
		})

	Register("vertex-anthropic", "Claude on Google Vertex AI (needs an ADC token)",
		func(s Spec) (Adapter, error) {
			if s.BaseURL == "" && (s.Project == "" || s.Region == "") {
				return nil, fmt.Errorf("vertex-anthropic needs either base_url, or " +
					"both project and region")
			}
			base := s.BaseURL
			if base == "" {
				base = fmt.Sprintf(
					"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic",
					s.Region, s.Project, s.Region)
			}
			if s.Model == "" {
				return nil, fmt.Errorf("vertex-anthropic needs a model")
			}
			a := NewAnthropic(base, s.APIKey, s.Model, anthropicProfile(s))
			a.Defaults = s.Params
			return a, nil
		})

	Register("vertex-gemini", "Gemini on Google Vertex AI (needs an ADC token)",
		func(s Spec) (Adapter, error) {
			if s.BaseURL == "" && (s.Project == "" || s.Region == "") {
				return nil, fmt.Errorf("vertex-gemini needs either base_url, or " +
					"both project and region")
			}
			base := s.BaseURL
			if base == "" {
				base = fmt.Sprintf(
					"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google",
					s.Region, s.Project, s.Region)
			}
			if s.Model == "" {
				return nil, fmt.Errorf("vertex-gemini needs a model")
			}
			g := NewGemini(base, s.APIKey, s.Model, Profile{
				Name:            s.Model,
				ContextWindow:   s.ContextWindow,
				MaxOutputTokens: s.MaxOutputTokens,
				SupportsTools:   true,
				SupportsVision:  true,
				SupportsStream:  true,
				ToolCallFormat:  "json",
				ReasoningTokens: true,
				Sampling: Sampling{
					Temperature: true, TopP: true, TopK: true, Seed: true,
					Stop: true, Think: true, ThinkingBudget: true,
				},
			})
			g.Defaults = s.Params
			return g, nil
		})
}

func anthropicProfile(s Spec) Profile {
	return Profile{
		Name:            s.Model,
		ContextWindow:   s.ContextWindow,
		MaxOutputTokens: s.MaxOutputTokens,
		SupportsTools:   true,
		SupportsVision:  true,
		SupportsStream:  true,
		ToolCallFormat:  "json",
		ReasoningTokens: true,
		CachePrefix:     true,
		Sampling: Sampling{
			Temperature: true, TopP: true, TopK: true, Stop: true,
			Think: true, ThinkingBudget: true, Effort: true,
		},
	}
}
