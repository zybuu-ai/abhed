package toolset

import (
	"fmt"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
)

// VerifyMode is the verify mode a surface runs with: the configured one, or
// with none configured label where a person reads the answer as it is given
// (interactive) and off elsewhere.
func VerifyMode(cfg config.Config, interactive bool) agent.VerifyMode {
	m, err := agent.ParseVerifyMode(cfg.Verify.Mode)
	if err != nil {
		return agent.VerifyStrict // Validate refuses this; fail toward checking
	}
	if cfg.Verify.Mode == "" && interactive {
		return agent.VerifyLabel
	}
	return m
}

// Verifier is the answer checker a surface gives its loop, or nil when the
// mode is off and no contract is configured. A critic whose model cannot be
// had is an error rather than silently no critic.
func Verifier(cfg config.Config, interactive bool) (*agent.Verifier, error) {
	mode := VerifyMode(cfg, interactive)
	v := cfg.Verify
	if mode == agent.VerifyOff && !v.Contract.On() {
		return nil, nil
	}
	out := &agent.Verifier{Mode: mode, AnswerTool: mode == agent.VerifyStrict}
	if v.AnswerTool != nil {
		out.AnswerTool = *v.AnswerTool
	}
	if v.Contract.On() {
		out.Contract = &agent.Contract{Require: v.Contract.Require, Protect: v.Contract.Protect, TimeoutMS: v.Contract.TimeoutMS}
	}
	if v.Critic.Enabled {
		c := &agent.Critic{Timeout: time.Duration(v.Critic.TimeoutMS) * time.Millisecond}
		if name := v.Critic.Model; name != "" {
			a, err := ModelResolver(cfg)(name)
			if err != nil {
				return nil, fmt.Errorf("verify.critic.model %q: %w", name, err)
			}
			c.Adapter = a
		}
		out.Critic = c
	}
	return out, nil
}

// VerifyFlag applies a -verify flag or an SDK option to cfg. Under a managed
// verify.mode it may only make checking stricter.
func VerifyFlag(cfg config.Config, mode string) (config.Config, error) {
	if mode == "" {
		return cfg, nil
	}
	m, err := agent.ParseVerifyMode(mode)
	if err != nil {
		return cfg, err
	}
	if cfg.ManagedSets("verify.mode") && config.VerifyRank(string(m)) < config.VerifyRank(cfg.Verify.Mode) {
		return cfg, &config.ManagedError{Key: "verify.mode", Value: string(m),
			Reason: fmt.Sprintf("the managed configuration sets it to %s, and a flag may only make it stricter", cfg.Verify.Mode),
			File:   managed.ConfigFile}
	}
	cfg.Verify.Mode = string(m)
	return cfg, nil
}
