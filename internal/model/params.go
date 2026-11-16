package model

import "fmt"

// Params carries the sampling and decoding controls for a request.
//
// Every field is a pointer, and that is the whole design. A model's own default
// for a knob is usually better than a number an adapter invents, and the
// providers disagree about what those defaults are — so "unset" has to be
// distinguishable from "set to zero". A plain float64 temperature makes an
// omitted setting indistinguishable from a deliberate 0.0, which is the
// difference between "leave it alone" and "make it fully deterministic".
//
// Not every provider honours every knob. Rather than silently dropping the ones
// it cannot express, an adapter reports what it supports (see Profile.Sampling)
// and Validate refuses a combination the endpoint will reject anyway, so a
// misconfiguration surfaces at startup instead of mid-session.
type Params struct {
	// Temperature scales the logits before sampling. 0 is greedy.
	Temperature *float64 `json:"temperature,omitempty"`
	// TopP is nucleus sampling: consider tokens covering this probability mass.
	TopP *float64 `json:"top_p,omitempty"`
	// TopK limits the candidate set to the k most likely tokens.
	TopK *int `json:"top_k,omitempty"`
	// MinP filters tokens below this fraction of the top token's probability.
	// Local servers (llama.cpp, vLLM) support it; hosted APIs generally do not.
	MinP *float64 `json:"min_p,omitempty"`
	// RepetitionPenalty divides the logits of tokens already produced.
	RepetitionPenalty *float64 `json:"repetition_penalty,omitempty"`
	// FrequencyPenalty and PresencePenalty are OpenAI's two-term formulation of
	// the same idea, kept separate because they compose differently.
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	// Seed makes sampling reproducible where the server supports it. This is
	// what makes an eval run comparable to the one before it.
	Seed *int64 `json:"seed,omitempty"`
	// MaxTokens caps the response. Zero means the adapter's own default.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Stop ends generation when any of these strings appears.
	Stop []string `json:"stop,omitempty"`
	// Effort is the reasoning budget for models that expose one.
	Effort EffortLevel `json:"effort,omitempty"`
	// Think turns a hybrid-reasoning model's thinking phase on or off. Ollama
	// reads this; an OpenAI-style server uses Effort instead.
	Think *bool `json:"think,omitempty"`
	// ThinkingBudget is Anthropic's and Gemini's token budget for extended
	// thinking, which is a count rather than a coarse level.
	ThinkingBudget *int `json:"thinking_budget,omitempty"`
}

// Sampling enumerates the knobs one provider actually honours, so a config can
// be rejected with a specific message instead of being quietly ignored.
type Sampling struct {
	Temperature       bool
	TopP              bool
	TopK              bool
	MinP              bool
	RepetitionPenalty bool
	FrequencyPenalty  bool
	PresencePenalty   bool
	Seed              bool
	Stop              bool
	Effort            bool
	Think             bool
	ThinkingBudget    bool
}

// SamplingOpenAI is the set an OpenAI-shaped API accepts.
func SamplingOpenAI() Sampling {
	return Sampling{
		Temperature: true, TopP: true, FrequencyPenalty: true,
		PresencePenalty: true, Seed: true, Stop: true, Effort: true,
	}
}

// SamplingLocal is a local server (vLLM, Ollama, llama.cpp, TGI), which
// accepts the OpenAI set plus the sampler knobs hosted APIs leave out.
func SamplingLocal() Sampling {
	s := SamplingOpenAI()
	s.TopK, s.MinP, s.RepetitionPenalty, s.Think = true, true, true, true
	return s
}

// Validate reports the parameters this provider cannot honour, and the ones
// that are outside their legal range.
//
// Refusing at startup matters more than it looks. A top_k sent to an endpoint
// that ignores it produces a run that looks configured and is not, and the
// evidence of that is nowhere in the output — the answers are simply drawn from
// a distribution nobody chose.
func (p Params) Validate(name string, s Sampling) error {
	var unsupported []string
	check := func(set bool, ok bool, knob string) {
		if set && !ok {
			unsupported = append(unsupported, knob)
		}
	}
	check(p.Temperature != nil, s.Temperature, "temperature")
	check(p.TopP != nil, s.TopP, "top_p")
	check(p.TopK != nil, s.TopK, "top_k")
	check(p.MinP != nil, s.MinP, "min_p")
	check(p.RepetitionPenalty != nil, s.RepetitionPenalty, "repetition_penalty")
	check(p.FrequencyPenalty != nil, s.FrequencyPenalty, "frequency_penalty")
	check(p.PresencePenalty != nil, s.PresencePenalty, "presence_penalty")
	check(p.Seed != nil, s.Seed, "seed")
	check(len(p.Stop) > 0, s.Stop, "stop")
	check(p.Effort != EffortNone, s.Effort, "effort")
	check(p.Think != nil, s.Think, "think")
	check(p.ThinkingBudget != nil, s.ThinkingBudget, "thinking_budget")

	if len(unsupported) > 0 {
		return fmt.Errorf("provider %q does not support %s — remove %s from its "+
			"params, or the endpoint will ignore it and the run will not be "+
			"configured the way it appears to be",
			name, joinAnd(unsupported), plural(len(unsupported), "it", "them"))
	}

	if err := inRange("temperature", p.Temperature, 0, 2); err != nil {
		return err
	}
	if err := inRange("top_p", p.TopP, 0, 1); err != nil {
		return err
	}
	if err := inRange("min_p", p.MinP, 0, 1); err != nil {
		return err
	}
	if err := inRange("frequency_penalty", p.FrequencyPenalty, -2, 2); err != nil {
		return err
	}
	if err := inRange("presence_penalty", p.PresencePenalty, -2, 2); err != nil {
		return err
	}
	if p.TopK != nil && *p.TopK < 0 {
		return fmt.Errorf("top_k must not be negative, got %d", *p.TopK)
	}
	if p.RepetitionPenalty != nil && *p.RepetitionPenalty <= 0 {
		return fmt.Errorf("repetition_penalty must be greater than 0, got %v",
			*p.RepetitionPenalty)
	}
	if p.ThinkingBudget != nil && *p.ThinkingBudget < 0 {
		return fmt.Errorf("thinking_budget must not be negative, got %d", *p.ThinkingBudget)
	}
	switch p.Effort {
	case EffortNone, EffortLow, EffortMedium, EffortHigh:
	default:
		return fmt.Errorf("effort must be low, medium or high, got %q", p.Effort)
	}
	return nil
}

func inRange(name string, v *float64, lo, hi float64) error {
	if v == nil {
		return nil
	}
	if *v < lo || *v > hi {
		return fmt.Errorf("%s must be between %v and %v, got %v", name, lo, hi, *v)
	}
	return nil
}

func joinAnd(items []string) string {
	switch len(items) {
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	}
	out := ""
	for i, s := range items[:len(items)-1] {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out + " and " + items[len(items)-1]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Merge overlays non-nil fields of other onto p and returns the result, so a
// per-request override composes with the configured defaults rather than
// replacing them wholesale.
func (p Params) Merge(other Params) Params {
	out := p
	if other.Temperature != nil {
		out.Temperature = other.Temperature
	}
	if other.TopP != nil {
		out.TopP = other.TopP
	}
	if other.TopK != nil {
		out.TopK = other.TopK
	}
	if other.MinP != nil {
		out.MinP = other.MinP
	}
	if other.RepetitionPenalty != nil {
		out.RepetitionPenalty = other.RepetitionPenalty
	}
	if other.FrequencyPenalty != nil {
		out.FrequencyPenalty = other.FrequencyPenalty
	}
	if other.PresencePenalty != nil {
		out.PresencePenalty = other.PresencePenalty
	}
	if other.Seed != nil {
		out.Seed = other.Seed
	}
	if other.MaxTokens != 0 {
		out.MaxTokens = other.MaxTokens
	}
	if len(other.Stop) > 0 {
		out.Stop = other.Stop
	}
	if other.Effort != EffortNone {
		out.Effort = other.Effort
	}
	if other.Think != nil {
		out.Think = other.Think
	}
	if other.ThinkingBudget != nil {
		out.ThinkingBudget = other.ThinkingBudget
	}
	return out
}

// ThinkingBudget is the thinking budget a request to a will carry: the
// request's own, else the operator's configured one. It is 0 when there is
// none, or when a's provider sends no budget (the Profile says which).
// Wrappers pass SamplingDefaults through to the adapter they wrap.
func ThinkingBudget(a Adapter, req Request) int {
	if !a.Profile().Sampling.ThinkingBudget {
		return 0
	}
	if p := SamplingDefaultsOf(a).Merge(req.Sampling()).ThinkingBudget; p != nil {
		return *p
	}
	return 0
}

// SamplingDefaults are the operator's configured sampling parameters.
func (c *Anthropic) SamplingDefaults() Params        { return c.Defaults }
func (g *Gemini) SamplingDefaults() Params           { return g.Defaults }
func (c *OpenAICompatible) SamplingDefaults() Params { return c.Defaults }
func (w *WatsonX) SamplingDefaults() Params          { return w.Defaults }

// SamplingDefaultsOf is a's configured sampling parameters, or none.
func SamplingDefaultsOf(a Adapter) Params {
	if s, ok := a.(interface{ SamplingDefaults() Params }); ok {
		return s.SamplingDefaults()
	}
	return Params{}
}
