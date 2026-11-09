package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
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
		Timeouts: model.Timeouts{
			Call:  time.Duration(p.CallTimeoutSeconds) * time.Second,
			Stall: time.Duration(p.StallTimeoutSeconds) * time.Second,
		},
		Extra: extra,
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
// started at all. With hooks.managed_only, the same holds for every extension
// the managed file did not configure.
func (c Config) ExtensionSpecs() []extension.Config {
	out := make([]extension.Config, 0, len(c.Extensions))
	for _, e := range c.Extensions {
		events := make([]extension.Event, 0, len(e.Events))
		for _, ev := range e.Events {
			events = append(events, extension.Event(ev))
		}
		if c.Hooks.Disabled || c.Hooks.ManagedOnly && c.ExtensionLayer(e.Name) != LayerManaged {
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

// NarrowHooks applies hooks.disabled and hooks.managed_only to extensions a
// caller supplies beside the configuration's, which are never the managed file's.
func (c Config) NarrowHooks(specs []extension.Config) []extension.Config {
	if !c.Hooks.Disabled && !c.Hooks.ManagedOnly {
		return specs
	}
	out := make([]extension.Config, 0, len(specs))
	for _, s := range specs {
		if s.Events = toolEventsOnly(s.Events); len(s.Events) > 0 {
			out = append(out, s)
		}
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

// joinHooks is the simple hooks below with a file's added: one of the same
// name replaces the earlier, so a later layer, the managed one last, holds it.
func joinHooks(below, added []HookConfig) []HookConfig {
	if added == nil {
		return below
	}
	out := make([]HookConfig, 0, len(below)+len(added))
	for _, h := range below {
		if !slices.ContainsFunc(added, func(a HookConfig) bool { return a.Name == h.Name }) {
			out = append(out, h)
		}
	}
	return append(out, added...)
}

// HookSpecs converts the simple hooks to the extension package's form.
// hooks.disabled leaves none, and hooks.managed_only only the managed file's.
func (c Config) HookSpecs() []extension.HookConfig {
	if c.Hooks.Disabled {
		return nil
	}
	out := make([]extension.HookConfig, 0, len(c.Hooks.Run))
	for _, h := range c.Hooks.Run {
		if c.Hooks.ManagedOnly && h.Layer != LayerManaged {
			continue
		}
		events := make([]extension.Event, 0, len(h.Events))
		for _, ev := range h.Events {
			events = append(events, extension.Event(ev))
		}
		out = append(out, extension.HookConfig{
			Name: h.Name, Events: events, Command: h.Command, URL: h.URL,
			Headers: h.Headers, Match: h.Match, Layer: h.Layer,
			Timeout: time.Duration(h.TimeoutMS) * time.Millisecond,
		})
	}
	return out
}

// validateHooks refuses a simple hook that could not run as written, so a
// hook meant to block is never silently missing.
func (c Config) validateHooks() error {
	seen := map[string]bool{}
	for _, e := range c.Extensions {
		seen[e.Name] = true
	}
	for _, h := range c.Hooks.Run {
		if h.Name == "" {
			return errors.New("hooks.run: each hook needs a name")
		}
		if seen[h.Name] {
			return fmt.Errorf("hooks.run: %q names another hook or an extension", h.Name)
		}
		seen[h.Name] = true
		if (h.Command == "") == (h.URL == "") {
			return fmt.Errorf("hooks.run %q: set one of command and url", h.Name)
		}
		if h.URL != "" {
			u, err := url.Parse(h.URL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return fmt.Errorf("hooks.run %q: url must be an http or https URL", h.Name)
			}
		}
		if len(h.Events) == 0 {
			return fmt.Errorf("hooks.run %q: list the events it takes", h.Name)
		}
		for _, ev := range h.Events {
			if !slices.Contains(extension.HookEvents, extension.Event(ev)) {
				names := make([]string, 0, len(extension.HookEvents))
				for _, e := range extension.HookEvents {
					names = append(names, string(e))
				}
				return fmt.Errorf("hooks.run %q: %q is not an event a hook takes; use %s", h.Name, ev, strings.Join(names, ", "))
			}
		}
		for _, m := range h.Match {
			if _, err := policy.ParseRule(m); err != nil {
				return fmt.Errorf("hooks.run %q: match: %w", h.Name, err)
			}
		}
		if h.TimeoutMS < 0 {
			return fmt.Errorf("hooks.run %q: timeout_ms may not be negative", h.Name)
		}
	}
	return nil
}
