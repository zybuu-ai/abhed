package model

import (
	"fmt"
	"sort"
	"strings"
)

// Spec is the provider-neutral description of one configured endpoint.
//
// It exists so the factory for a provider takes a single argument that the
// config package can fill in without either package importing the other.
type Spec struct {
	Type            string
	BaseURL         string
	Model           string
	APIKey          string
	ContextWindow   int
	MaxOutputTokens int
	ToolCallFormat  string
	ReasoningTags   []string
	Params          Params
	// Timeouts bound each request; a zero field keeps its default.
	Timeouts Timeouts

	// Region and Project scope a cloud-hosted deployment: an AWS region, a
	// Google project, an Azure resource. Which of them a provider needs is the
	// provider's business.
	Region  string
	Project string

	// Extra carries the settings only one provider understands — watsonx's
	// space_id and api_version, Azure's deployment name, Vertex's location.
	// Keeping them here rather than as fields on every Spec stops this struct
	// growing a column per vendor.
	Extra map[string]string
}

// Get reads an Extra value.
func (s Spec) Get(key string) string { return s.Extra[key] }

// Factory builds an adapter from a spec.
type Factory func(Spec) (Adapter, error)

type provider struct {
	name    string
	summary string
	build   Factory
}

var providers = map[string]provider{}

// Register adds a provider under a config "type". Called from each adapter's
// init, so the set of providers is the set of files compiled in — which is what
// makes an air-gapped build able to drop the cloud ones without editing a
// factory.
func Register(name, summary string, build Factory) {
	providers[name] = provider{name: name, summary: summary, build: build}
}

// New builds the adapter for a spec, validating its parameters against what the
// provider actually honours.
func New(s Spec) (Adapter, error) {
	if s.Timeouts.Call < 0 || s.Timeouts.Stall < 0 {
		return nil, fmt.Errorf("call_timeout_seconds and stall_timeout_seconds may not be negative")
	}
	p, ok := providers[s.Type]
	if !ok {
		return nil, fmt.Errorf("unknown provider type %q; known types are %s",
			s.Type, strings.Join(Providers(), ", "))
	}
	a, err := p.build(s)
	if err != nil {
		return nil, err
	}
	if t, ok := a.(interface{ SetTimeouts(Timeouts) }); ok && s.Timeouts != (Timeouts{}) {
		t.SetTimeouts(s.Timeouts)
	}
	if err := s.Params.Validate(s.Type, a.Profile().Sampling); err != nil {
		return nil, err
	}
	return a, nil
}

// Providers lists the registered type names.
func Providers() []string {
	out := make([]string, 0, len(providers))
	for n := range providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Describe lists each provider with its one-line summary, for `abhed providers`.
func Describe() []string {
	out := make([]string, 0, len(providers))
	for _, n := range Providers() {
		out = append(out, fmt.Sprintf("%-20s %s", n, providers[n].summary))
	}
	return out
}

// Known reports whether a type is registered.
func Known(name string) bool {
	_, ok := providers[name]
	return ok
}

// splitList parses a comma-separated Extra value.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LoadCustom registers providers described by a file rather than compiled in.
//
// Adding a provider should not need a rebuild. Every endpoint worth reaching
// speaks one of three wire formats, and which one is a fact about the endpoint
// that an operator already knows — so it belongs in configuration, not in a Go
// file that has to be edited, reviewed and released.
//
// The wire format is named explicitly rather than guessed from the URL. A
// wrong guess produces requests that are rejected for reasons that point
// nowhere near the cause.
func LoadCustom(defs []CustomProvider) []error {
	var errs []error
	for _, d := range defs {
		if d.Name == "" {
			errs = append(errs, fmt.Errorf("custom provider: name is required"))
			continue
		}
		if Known(d.Name) {
			errs = append(errs, fmt.Errorf("custom provider %q: that name is already "+
				"a built-in provider; choose another", d.Name))
			continue
		}
		var factory Factory
		switch strings.ToLower(d.API) {
		case "openai", "openai-compatible", "":
			factory = openAIStyle(d.BaseURL, samplingFor(d.Sampling, SamplingLocal()))
		case "anthropic":
			factory = anthropicStyle(d.BaseURL)
		case "gemini":
			factory = geminiStyle(d.BaseURL)
		default:
			errs = append(errs, fmt.Errorf("custom provider %q: unknown api %q "+
				"(want openai, anthropic or gemini)", d.Name, d.API))
			continue
		}
		Register(d.Name, orElse(d.Description, "custom provider ("+d.API+")"), factory)
	}
	return errs
}

// CustomProvider is one entry in the providers file.
type CustomProvider struct {
	Name        string `json:"name"`
	API         string `json:"api"` // openai | anthropic | gemini
	BaseURL     string `json:"base_url"`
	Description string `json:"description,omitempty"`
	// Sampling names the knobs this endpoint honours. Empty means the
	// permissive local set, since a self-hosted server usually accepts them.
	Sampling []string `json:"sampling,omitempty"`
}

func samplingFor(names []string, fallback Sampling) Sampling {
	if len(names) == 0 {
		return fallback
	}
	var s Sampling
	for _, n := range names {
		switch strings.ToLower(strings.TrimSpace(n)) {
		case "temperature":
			s.Temperature = true
		case "top_p":
			s.TopP = true
		case "top_k":
			s.TopK = true
		case "min_p":
			s.MinP = true
		case "repetition_penalty":
			s.RepetitionPenalty = true
		case "frequency_penalty":
			s.FrequencyPenalty = true
		case "presence_penalty":
			s.PresencePenalty = true
		case "seed":
			s.Seed = true
		case "stop":
			s.Stop = true
		case "effort":
			s.Effort = true
		case "think":
			s.Think = true
		case "thinking_budget":
			s.ThinkingBudget = true
		}
	}
	return s
}

// visionFromSpec lets an operator declare that a local endpoint can see.
//
// The OpenAI-compatible family covers both hosted vision models and local
// text-only servers, so it cannot have a sensible default — and guessing wrong
// in the permissive direction means images silently ignored. Off unless the
// config says otherwise, using the same Extra escape hatch that already
// carries api_version and beta flags:
//
//	"extra": { "vision": "true" }
func visionFromSpec(s Spec) bool {
	v := s.Get("vision")
	return v == "true" || v == "1" || v == "yes"
}
