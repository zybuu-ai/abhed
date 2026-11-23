package model

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/zybuu-ai/abhed/internal/defaults"
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
	registerOpenAI("openai", "OpenAI (api.openai.com) or any drop-in replacement",
		parallelCalls(openAIStyle("https://api.openai.com/v1", SamplingOpenAI())))

	// The local servers accept the OpenAI body plus top_k, min_p and
	// repetition_penalty, which hosted APIs do not define.
	registerOpenAI("vllm", "vLLM server (OpenAI-compatible, /v1)",
		parallelCalls(openAIStyle(localURLs["vllm"], SamplingLocal())))
	registerOpenAI("ollama", "Ollama (OpenAI-compatible endpoint, /v1)", func(sp Spec) (Adapter, error) {
		return openAIStyle(LocalDefaultURL("ollama"), SamplingLocal())(sp)
	})
	registerOpenAI("llamacpp", "llama.cpp server (OpenAI-compatible, /v1)",
		openAIStyle(localURLs["llamacpp"], SamplingLocal()))
	registerOpenAI("tgi", "Hugging Face Text Generation Inference (/v1)",
		openAIStyle(localURLs["tgi"], SamplingLocal()))
	registerOpenAI("sglang", "SGLang server (OpenAI-compatible, /v1)",
		openAIStyle(localURLs["sglang"], SamplingLocal()))

	// Hosted OpenAI-compatible gateways.
	registerOpenAI("groq", "Groq (OpenAI-compatible)",
		openAIStyle("https://api.groq.com/openai/v1", SamplingOpenAI()))
	registerOpenAI("together", "Together AI (OpenAI-compatible)",
		openAIStyle("https://api.together.xyz/v1", SamplingLocal()))
	registerOpenAI("openrouter", "OpenRouter (OpenAI-compatible, many upstreams)",
		openAIStyle("https://openrouter.ai/api/v1", SamplingOpenAI()))
	registerOpenAI("mistral", "Mistral La Plateforme (OpenAI-compatible)",
		openAIStyle("https://api.mistral.ai/v1", SamplingOpenAI()))
	registerOpenAI("deepseek", "DeepSeek (OpenAI-compatible)",
		openAIStyle("https://api.deepseek.com/v1", SamplingOpenAI()))
	registerOpenAI("xai", "xAI Grok (OpenAI-compatible)",
		openAIStyle("https://api.x.ai/v1", SamplingOpenAI()))

	// The original name, kept so existing configs keep working. It assumes the
	// permissive local sampling set because that is what it was used for.
	registerOpenAI("openai-compatible", "any OpenAI-compatible endpoint (generic)",
		parallelCalls(openAIStyle("", SamplingLocal())))
}

// localURLs are the endpoints local server types use when no base_url is set.
var localURLs = map[string]string{"vllm": "http://127.0.0.1:8000/v1", "llamacpp": "http://127.0.0.1:8080/v1",
	"tgi": "http://127.0.0.1:8080/v1", "sglang": "http://127.0.0.1:30000/v1"}

// LocalDefaultURL is the endpoint a local server type uses with no
// base_url, as its adapter resolves it; "" for any other type.
func LocalDefaultURL(typ string) string {
	if typ == "ollama" {
		return defaults.OllamaURL
	}
	return localURLs[typ]
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
			key = s.Get("oauth_token")
			if key == "" && !s.NoAmbientKey {
				key = os.Getenv("OPENAI_API_KEY")
			}
		}
		profile.StreamsReasoning = streamsReasoning(s.Type, base)
		// The official API's reasoning models bill reasoning they never stream.
		profile.ReasoningTokens = s.Type == "openai" && hiddenReasoning(s.Model)
		a := NewOpenAICompatible(base, key, s.Model, profile)
		a.Defaults = s.Params
		a.Think = s.Params.Think
		a.User = s.Get("user")
		cache, err := promptCacheMode(s, base, defaultURL)
		if err != nil {
			return nil, err
		}
		a.PromptCache = cache
		a.ParallelToolCalls = s.Get("parallel_tool_calls") == "true"
		a.EffortOff, err = effortOff(s, base)
		if err != nil {
			return nil, err
		}
		if len(s.ReasoningTags) == 2 {
			a.ReasoningTags = [2]string{s.ReasoningTags[0], s.ReasoningTags[1]}
		}
		return a, nil
	}
}

// streamsReasoning is a local server known to stream reasoning: Ollama and
// llama.cpp anywhere, and vLLM, TGI or SGLang on a loopback address. A
// generic or hosted entry may front a model that reasons out of sight.
func streamsReasoning(typ, base string) bool {
	switch typ {
	case "ollama", "llamacpp":
		return true
	case "vllm", "tgi", "sglang":
		return loopback(base)
	}
	return false
}

// hiddenReasoning is an OpenAI reasoning model: the o-series and gpt-5 on.
func hiddenReasoning(model string) bool {
	m := strings.ToLower(model)
	return len(m) > 1 && m[0] == 'o' && m[1] >= '1' && m[1] <= '9' || strings.HasPrefix(m, "gpt-5") || strings.HasPrefix(m, "gpt-6")
}

// loopback reports a base URL on this machine's loopback address.
func loopback(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// effortOff says whether thinking off is also sent as reasoning_effort
// "none": for Ollama (its type, or an openai-compatible entry on Ollama's
// loopback port 11434), or as extra.think_off ("reasoning_effort", or
// "think" for the field alone), which a remote Ollama needs.
func effortOff(s Spec, base string) (bool, error) {
	switch v := s.Get("think_off"); v {
	case "reasoning_effort":
		return true, nil
	case "think":
		return false, nil
	case "":
	default:
		return false, fmt.Errorf("provider %q: extra.think_off must be reasoning_effort or think, not %q", s.Type, v)
	}
	if s.Type == "ollama" {
		return true, nil
	}
	u, err := url.Parse(base)
	return err == nil && s.Type == "openai-compatible" && u.Port() == "11434" && loopback(base), nil
}

// parallelCalls is f for a server known to take "parallel_tool_calls": it is
// sent unless the provider's extra.parallel_tool_calls is "false".
func parallelCalls(f Factory) Factory {
	return func(s Spec) (Adapter, error) {
		a, err := f(s)
		if c, ok := a.(*OpenAICompatible); ok && err == nil && s.Get("parallel_tool_calls") != "false" {
			c.ParallelToolCalls = true
		}
		return a, err
	}
}

// promptCacheMode is how a provider is asked to cache its prompt: OpenAI's
// own API takes prompt_cache_key, and OpenRouter takes cache_control for its
// Anthropic and Gemini models. Any other server gets neither field unless
// extra.prompt_cache names one ("prompt_cache_key", "cache_control" or
// "off"), since a server that does not know a field may refuse the request.
func promptCacheMode(s Spec, base, defaultURL string) (string, error) {
	switch v := s.Get("prompt_cache"); v {
	case "off":
		return "", nil
	case CacheKey, CacheControl:
		return v, nil
	case "", "auto":
	default:
		return "", fmt.Errorf("provider %q: extra.prompt_cache must be prompt_cache_key, cache_control, auto or off, not %q", s.Type, v)
	}
	official := sameEndpoint(base, defaultURL)
	switch {
	case s.Type == "openai" && official:
		return CacheKey, nil
	case s.Type == "openrouter" && official && (strings.HasPrefix(s.Model, "anthropic/") || strings.HasPrefix(s.Model, "google/gemini")):
		return CacheControl, nil
	}
	return "", nil
}

// sameEndpoint compares two https URLs as a server would: host without case,
// :443 as no port, a trailing slash as none.
func sameEndpoint(a, b string) bool {
	norm := func(raw string) string {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Scheme != "https" {
			return raw
		}
		host := strings.ToLower(u.Hostname())
		if p := u.Port(); p != "" && p != "443" {
			host += ":" + p
		}
		return "https://" + host + strings.TrimSuffix(u.EscapedPath(), "/")
	}
	return norm(a) == norm(b)
}

func orElse(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
