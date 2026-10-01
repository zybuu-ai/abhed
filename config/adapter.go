package config

import (
	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/model"
)

// Spec converts a provider config into the model package's neutral form.
//
// This lives in config rather than model so that model stays free of any
// knowledge of the file format, and config stays free of any knowledge of how
// a given provider is built. The registry in between is what lets a new
// provider be one new file with an init, rather than an edit to a factory
// switch in main.
func (p ProviderConfig) Spec() model.Spec {
	extra := map[string]string{}
	for k, v := range p.Extra {
		extra[k] = v
	}
	// The watsonx fields predate Extra and stay first-class in the config file;
	// carry them through so one provider's settings do not become a special
	// case in the model package.
	if p.SpaceID != "" {
		extra["space_id"] = p.SpaceID
	}
	if p.APIVersion != "" {
		extra["api_version"] = p.APIVersion
	}
	if p.IAMURL != "" {
		extra["iam_url"] = p.IAMURL
	}

	return model.Spec{
		Type:            p.Type,
		BaseURL:         p.BaseURL,
		Model:           p.Model,
		APIKey:          p.APIKey,
		ContextWindow:   p.ContextWindow,
		MaxOutputTokens: p.MaxOutputTokens,
		ToolCallFormat:  p.ToolCallFormat,
		ReasoningTags:   p.ReasoningTags,
		Region:          p.Region,
		Project:         p.ProjectID,
		Params:          p.Params.Model(p.Think),
		Extra:           extra,
	}
}

// Model converts the config form of the sampling parameters.
//
// think is the older top-level provider field, kept working: an explicit
// params.think wins, and the legacy field applies when params does not set it.
func (c ParamsConfig) Model(think *bool) model.Params {
	out := model.Params{
		Temperature:       c.Temperature,
		TopP:              c.TopP,
		TopK:              c.TopK,
		MinP:              c.MinP,
		RepetitionPenalty: c.RepetitionPenalty,
		FrequencyPenalty:  c.FrequencyPenalty,
		PresencePenalty:   c.PresencePenalty,
		Seed:              c.Seed,
		MaxTokens:         c.MaxTokens,
		Stop:              c.Stop,
		Effort:            model.EffortLevel(c.Effort),
		Think:             c.Think,
		ThinkingBudget:    c.ThinkingBudget,
	}
	if out.Think == nil {
		out.Think = think
	}
	return out
}

// Adapter builds the model adapter this provider describes, validating its
// parameters against what the provider actually honours.
func (p ProviderConfig) Adapter() (model.Adapter, error) {
	return model.New(p.Spec())
}

// ExtensionSpecs converts the configured extensions to the extension package's
// form, so neither package needs to import the other's types.
//
// With hooks.disabled, which only the managed configuration sets, an
// extension keeps only the tools it provides: it is sent list_tools and
// invoke_tool and no hook event, and one that provides no tools is not
// started at all.
func (c Config) ExtensionSpecs() []extension.Config {
	out := make([]extension.Config, 0, len(c.Extensions))
	for _, e := range c.Extensions {
		events := make([]extension.Event, 0, len(e.Events))
		for _, ev := range e.Events {
			events = append(events, extension.Event(ev))
		}
		if c.Hooks.Disabled {
			if events = toolEventsOnly(events); len(events) == 0 {
				continue
			}
		}
		out = append(out, extension.Config{
			Name: e.Name, Command: e.Command, Args: e.Args,
			Events: events, TimeoutMS: e.TimeoutMS, Env: e.Env,
			Match: e.Match, Async: e.Async,
		})
	}
	return out
}

// toolEventsOnly keeps the events that serve an extension's own tools. No
// events means every event, so it becomes those two.
func toolEventsOnly(events []extension.Event) []extension.Event {
	if len(events) == 0 {
		return []extension.Event{extension.EvListTools, extension.EvInvokeTool}
	}
	var out []extension.Event
	for _, ev := range events {
		if ev == extension.EvListTools || ev == extension.EvInvokeTool {
			out = append(out, ev)
		}
	}
	return out
}

// RegisterCustomProviders adds the configured providers to the model registry.
// Called before any provider is resolved, so a custom name is usable as
// model.default.
func (c Config) RegisterCustomProviders() []error {
	defs := make([]model.CustomProvider, 0, len(c.CustomProviders))
	for _, p := range c.CustomProviders {
		defs = append(defs, model.CustomProvider{
			Name: p.Name, API: p.API, BaseURL: p.BaseURL,
			Description: p.Description, Sampling: p.Sampling,
		})
	}
	return model.LoadCustom(defs)
}
