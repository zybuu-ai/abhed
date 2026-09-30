package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/internal/model"
)

// Choosing the model from the console.
//
// Twenty providers ship, hosted and on-prem alike, behind one abstraction —
// and switching between them meant editing a config file and restarting. That
// hid the thing Abhed is actually built on: the harness is the product, and the
// model is a swappable input. Being able to run the same task against four
// models and watch the harness hold steady is the argument, made visible.
//
// The rule that matters: a client names a provider from the CONFIGURED set. It
// never supplies a URL, a key, or a model string. Accepting those would let a
// session point the agent at an attacker-controlled endpoint — every prompt,
// every file the agent had read, delivered to a chosen host — and would move
// credentials from the server's environment into a request body.

// providerInfo is one selectable model, as the console sees it.
type providerInfo struct {
	Name          string `json:"name"`
	Model         string `json:"model"`
	Type          string `json:"type"`
	ContextWindow int    `json:"context_window,omitempty"`
	Default       bool   `json:"default"`
}

// providers lists what this deployment can switch between.
func (s *Server) providers() []providerInfo {
	cfg := s.opts.Config
	out := make([]providerInfo, 0, len(cfg.Model.Providers))
	for name, p := range cfg.Model.Providers {
		// A built-in the configuration never named is not offered.
		if !cfg.Offered(name) {
			continue
		}
		out = append(out, providerInfo{
			Name: name, Model: p.Model, Type: p.Type,
			ContextWindow: p.ContextWindow,
			Default:       name == cfg.Model.Default,
		})
	}
	// Stable order, or the dropdown reshuffles on every poll.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// listProviders answers the console's model picker.
func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, s.providers())
}

// resolveProvider turns a client-supplied NAME into an adapter.
//
// The lookup is against the configured map, so an unknown name is refused
// rather than treated as an endpoint. config.ProviderConfig.Adapter() resolves
// APIKeyEnv from the server's environment, which is what keeps the credential
// server-side.
func (s *Server) resolveProvider(name string) (model.Adapter, error) {
	// Only a listed provider runs a session; a built-in nobody configured is not one.
	if !s.opts.Config.Offered(name) {
		return nil, errUnknownProvider
	}
	p, err := s.opts.Config.ProviderNamed(name)
	if err != nil {
		return nil, errUnknownProvider
	}
	a, err := p.Adapter()
	if err != nil {
		return nil, err
	}
	return a, nil
}

// providerNames are the providers a session may run on, sorted.
func (s *Server) providerNames() []string {
	var out []string
	for _, p := range s.providers() {
		out = append(out, p.Name)
	}
	return out
}

// subagentModel resolves a subagent's model as resolveProvider does, and
// says what may be chosen instead when it cannot.
func (s *Server) subagentModel(name string) (model.Adapter, error) {
	a, err := s.resolveProvider(name)
	if err != nil {
		return nil, fmt.Errorf("%w; available: %s", err, strings.Join(s.providerNames(), ", "))
	}
	return a, nil
}

type providerError string

func (e providerError) Error() string { return string(e) }

const errUnknownProvider providerError = "no such provider is configured"

// setSessionModel swaps the model on a session, recording the switch so the
// record and a later resume both name the model that answers.
func (s *Server) setSessionModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validSessionID(id) {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if s.draining.Load() {
		w.Header().Set("Retry-After", "5")
		WriteError(w, http.StatusServiceUnavailable, errDraining.Error())
		return
	}
	// A session finished before a restart is reopened, as a message would
	// continue it, rather than refused as unknown.
	live, status, msg := s.liveOrReopened(r, id)
	if live == nil {
		if status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "5")
		}
		WriteError(w, status, msg)
		return
	}

	adapter, err := s.resolveProvider(req.Provider)
	if err != nil {
		// Named separately from a 404 on the session: "that provider is not
		// configured" is a different fix from "that session is not yours".
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Held as a message holds it until its turn runs, so no turn can start
	// between the check below and the swap: the loop goroutine reads the adapter.
	live.claimMu.Lock()
	defer live.claimMu.Unlock()
	// Again under claimMu: a claim taken once a drain began would be held past it.
	if s.draining.Load() {
		w.Header().Set("Retry-After", "5")
		WriteError(w, http.StatusServiceUnavailable, errDraining.Error())
		return
	}
	newly, err := s.claimLocked(r.Context(), id, live)
	switch {
	case errors.Is(err, errBusySession):
		WriteError(w, http.StatusConflict, "session is being continued elsewhere")
		return
	case errors.Is(err, errHoldFailed):
		writeHoldFailed(w)
		return
	case err != nil:
		s.log.Error("claim failed", "session", id, "error", err)
		WriteError(w, http.StatusInternalServerError, "could not switch the session's model")
		return
	}
	if newly {
		s.holdClaim(id, live) // no turn follows, so the claim is kept as a workbench write keeps it
	}
	live.mu.Lock()
	busy := live.State == "running" || live.State == "waiting_approval"
	from := live.model
	live.mu.Unlock()
	if busy {
		WriteError(w, http.StatusConflict,
			"the session is mid-turn; interrupt it or wait for the turn to finish")
		return
	}
	// Recorded before it takes effect: a model the record cannot name must not answer.
	if err := live.Loop.SwitchModel(req.Provider, adapter); err != nil {
		s.log.Error("model switch not recorded", "session", id, "error", err)
		WriteError(w, http.StatusInternalServerError, "the model was not changed: the switch could not be recorded")
		return
	}
	live.fallback = nil // the switch just recorded says where the session is
	live.mu.Lock()
	live.provider, live.model = req.Provider, adapter.Profile().Name
	live.mu.Unlock()

	s.log.Info("session model changed", "session", id,
		"provider", req.Provider, "user", UserOf(r.Context()))
	WriteJSON(w, http.StatusOK, map[string]string{
		"provider": req.Provider,
		"model":    adapter.Profile().Name,
		"from":     from,
	})
}
