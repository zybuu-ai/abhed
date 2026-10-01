package abhed

import (
	"errors"
	"fmt"
	"sort"
)

// Model is one configured model an agent may be switched to. It carries no
// endpoint, key or variable name: a caller chooses by Name and nothing else.
type Model struct {
	Name    string // the configured provider name
	Model   string // the model id it serves
	Type    string // the provider type: ollama, openai-compatible, …
	Current bool   // the model the agent runs on now
}

// ErrUnknownModel is SwitchModelNamed's refusal of a name that is not offered.
var ErrUnknownModel = errors.New("abhed: no such model is configured")

// ErrSwitchDuringRun is SwitchModelNamed's refusal while a run is in progress.
var ErrSwitchDuringRun = errors.New("abhed: cannot switch the model while a run is in progress; switch after it returns")

// offered reports whether a configured provider may be chosen by name. A
// managed file that sets model.default pins the model to it.
func (a *Agent) offered(name string) bool {
	if _, ok := a.cfg.Model.Providers[name]; !ok || !a.cfg.Offered(name) {
		return false
	}
	return !a.cfg.ManagedSets("model.default") || name == a.cfg.Model.Default
}

// Models lists the configured models SwitchModelNamed accepts, by name. Only
// what a trusted configuration file defines is here: an untrusted workspace
// file adds no provider.
func (a *Agent) Models() []Model {
	a.forkMu.Lock()
	current := a.current
	a.forkMu.Unlock()
	out := make([]Model, 0, len(a.cfg.Model.Providers))
	for name, p := range a.cfg.Model.Providers {
		if !a.offered(name) {
			continue
		}
		out = append(out, Model{Name: name, Model: p.Model, Type: p.Type, Current: name == current})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SwitchModelNamed moves the conversation to the configured provider name,
// keeping the history, and records model.switched first. The name is looked
// up in the configuration, never used as an endpoint. It returns
// ErrUnknownModel for a name Models does not list, ErrSwitchDuringRun while a
// run is in progress, and an error naming the variable when the provider's
// api_key_env is unset. A run started meanwhile waits for the switch.
func (a *Agent) SwitchModelNamed(name string) error {
	a.forkMu.Lock()
	defer a.forkMu.Unlock()
	if a.running > 0 {
		return ErrSwitchDuringRun
	}
	if !a.offered(name) {
		return fmt.Errorf("%w: %q", ErrUnknownModel, name)
	}
	if name == a.current {
		return nil
	}
	p, err := a.cfg.ProviderNamed(name)
	if err != nil {
		return fmt.Errorf("abhed: %s", redactText(a.redact, err.Error()))
	}
	if p.APIKey == "" && p.APIKeyEnv != "" {
		return fmt.Errorf("abhed: model %q reads its key from %s, which is not set", name, p.APIKeyEnv)
	}
	next, err := p.Adapter()
	if err != nil {
		return fmt.Errorf("abhed: model %q: %s", name, redactText(a.redact, err.Error()))
	}
	if err := a.loop.SwitchModel(name, next); err != nil {
		return fmt.Errorf("abhed: the model was not changed: the switch could not be recorded: %w", err)
	}
	a.current = name
	return nil
}
