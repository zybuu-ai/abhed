package model

import (
	"fmt"
	"os"
)

// Registrations for every provider that speaks the OpenAI chat-completions
// wire format. They differ in base URL, in which sampler knobs the server
// honours, and in nothing else — so they share one adapter and differ only in
// the spec each one fills in.
//
// They are separate config types rather than one "openai-compatible" because
// the differences are real and silent: min_p sent to OpenAI is ignored, and a
// user who wrote it in a config deserves to be told at startup rather than to
// wonder why the setting had no effect.
func init() {
	Register("openai", "OpenAI (api.openai.com) or any drop-in replacement",
		openAIStyle("https://api.openai.com/v1", SamplingOpenAI()))

	// The local servers accept the OpenAI body plus top_k, min_p and
	// repetition_penalty, which hosted APIs do not define.
	Register("vllm", "vLLM server (OpenAI-compatible, /v1)",
		openAIStyle("http://127.0.0.1:8000/v1", SamplingLocal()))
	Register("ollama", "Ollama (OpenAI-compatible endpoint, /v1)",
		openAIStyle("http://127.0.0.1:11434/v1", SamplingLocal()))
	Register("llamacpp", "llama.cpp server (OpenAI-compatible, /v1)",
		openAIStyle("http://127.0.0.1:8080/v1", SamplingLocal()))
	Register("tgi", "Hugging Face Text Generation Inference (/v1)",
		openAIStyle("http://127.0.0.1:8080/v1", SamplingLocal()))
	Register("sglang", "SGLang server (OpenAI-compatible, /v1)",
		openAIStyle("http://127.0.0.1:30000/v1", SamplingLocal()))

	// Hosted OpenAI-compatible gateways.
	Register("groq", "Groq (OpenAI-compatible)",
		openAIStyle("https://api.groq.com/openai/v1", SamplingOpenAI()))
	Register("together", "Together AI (OpenAI-compatible)",
		openAIStyle("https://api.together.xyz/v1", SamplingLocal()))
	Register("openrouter", "OpenRouter (OpenAI-compatible, many upstreams)",
		openAIStyle("https://openrouter.ai/api/v1", SamplingOpenAI()))
	Register("mistral", "Mistral La Plateforme (OpenAI-compatible)",
		openAIStyle("https://api.mistral.ai/v1", SamplingOpenAI()))
	Register("deepseek", "DeepSeek (OpenAI-compatible)",
		openAIStyle("https://api.deepseek.com/v1", SamplingOpenAI()))
	Register("xai", "xAI Grok (OpenAI-compatible)",
		openAIStyle("https://api.x.ai/v1", SamplingOpenAI()))

	// The original name, kept so existing configs keep working. It assumes the
	// permissive local sampling set because that is what it was used for.
	Register("openai-compatible", "any OpenAI-compatible endpoint (generic)",
		openAIStyle("", SamplingLocal()))
}

func openAIStyle(defaultURL string, sampling Sampling) Factory {
	return func(s Spec) (Adapter, error) {
		base := s.BaseURL
		if base == "" {
			base = defaultURL
		}
		if base == "" {
			return nil, fmt.Errorf("provider %q needs a base_url", s.Type)
		}
		if s.Model == "" {
			return nil, fmt.Errorf("provider %q needs a model", s.Type)
		}
		profile := Profile{
			Name:            s.Model,
			ContextWindow:   s.ContextWindow,
			MaxOutputTokens: s.MaxOutputTokens,
			SupportsTools:   true,
			SupportsVision:  visionFromSpec(s),
			SupportsStream:  true,
			ToolCallFormat:  orElse(s.ToolCallFormat, "json"),
			Sampling:        sampling,
		}
		key := s.APIKey
		if key == "" {
			// An OpenAI-shaped endpoint authenticates with a bearer token
			// either way, so a subscription token and an API key take the same
			// path here. Only the source differs.
			key = firstNonEmptyString(
				s.Get("oauth_token"),
				os.Getenv("OPENAI_API_KEY"),
			)
		}
		a := NewOpenAICompatible(base, key, s.Model, profile)
		a.Defaults = s.Params
		a.Think = s.Params.Think
		a.User = s.Get("user")
		if len(s.ReasoningTags) == 2 {
			a.ReasoningTags = [2]string{s.ReasoningTags[0], s.ReasoningTags[1]}
		}
		return a, nil
	}
}

func orElse(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
