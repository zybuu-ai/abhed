package config

import (
	"fmt"
	"strings"
)

// VerifyConfig governs answer verification: whether each answer is checked
// against the session's record, and what must hold before a run completes.
type VerifyConfig struct {
	// Mode is off, label or strict. Unset lets the surface choose: label in
	// the terminal, the workbench and Abhed Studio, where a person reads the
	// answer; off for -p, rpc, schedules and the SDK unless they ask.
	Mode string `json:"mode,omitempty"`
	// AnswerTool offers the agent the answer tool, which carries each claim's
	// evidence. Unset is on in strict mode and off in label mode.
	AnswerTool *bool `json:"answer_tool,omitempty"`
	// Critic has a model check the verified claims; it may only downgrade.
	Critic VerifyCriticConfig `json:"critic,omitempty"`
	// Contract is what must hold before a run may end completed.
	Contract ContractConfig `json:"contract,omitempty"`
}

// VerifyCriticConfig turns on the critic and names its model.
type VerifyCriticConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Model names a configured provider; unset asks the session's own.
	Model string `json:"model,omitempty"`
	// TimeoutMS bounds the critic's call; unset is 30,000.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// ContractConfig is the operator's completion contract.
type ContractConfig struct {
	// Require are commands the harness runs in the session's sandbox after
	// the agent's last change; each must exit 0.
	Require []string `json:"require,omitempty"`
	// Protect are globs of files a run must not change.
	Protect []string `json:"protect,omitempty"`
	// TimeoutMS bounds each required command; unset is the bash tool's default.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// On reports whether the contract asks for anything.
func (c ContractConfig) On() bool { return len(c.Require) > 0 || len(c.Protect) > 0 }

var verifyRank = map[string]int{"off": 0, "label": 1, "strict": 2}

// VerifyRank orders verify modes, off below label below strict; -1 for an
// unknown one. Unset ranks as label, the interactive default, so a workspace
// may raise it but never take it below what a person would see.
func VerifyRank(mode string) int {
	if mode == "" {
		return verifyRank["label"]
	}
	if r, ok := verifyRank[strings.ToLower(mode)]; ok {
		return r
	}
	return -1
}

func (c Config) validateVerify() error {
	v := c.Verify
	if v.Mode != "" && VerifyRank(v.Mode) < 0 {
		return fmt.Errorf("verify.mode is %q; use off, label or strict", v.Mode)
	}
	if m := v.Critic.Model; m != "" {
		if _, found := c.Model.Providers[m]; !found {
			return fmt.Errorf("verify.critic.model is %q, which is not a configured provider; available: %s",
				m, strings.Join(providerNames(c.Model.Providers), ", "))
		}
	}
	if v.Critic.TimeoutMS < 0 || v.Contract.TimeoutMS < 0 {
		return fmt.Errorf("verify timeouts are milliseconds, 0 for the default")
	}
	for _, cmd := range v.Contract.Require {
		if strings.TrimSpace(cmd) == "" {
			return fmt.Errorf("verify.contract.require has an empty command")
		}
	}
	for _, g := range v.Contract.Protect {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("verify.contract.protect has an empty pattern")
		}
	}
	return nil
}

// stricterVerify takes a workspace's verify.mode only when it checks at
// least as much as the current one.
func stricterVerify(dst, ws *Config) bool {
	r := VerifyRank(ws.Verify.Mode)
	if ws.Verify.Mode == "" || r < 0 || r < VerifyRank(dst.Verify.Mode) {
		return false
	}
	dst.Verify.Mode = strings.ToLower(ws.Verify.Mode)
	return true
}

func answerToolOn(dst, ws *Config) bool {
	if ws.Verify.AnswerTool == nil || !*ws.Verify.AnswerTool {
		return false
	}
	on := true
	dst.Verify.AnswerTool = &on
	return true
}

func init() {
	workspaceRules["verify.mode"] = fieldRule{stricterVerify, "only stricter: off < label < strict"}
	workspaceRules["verify.answer_tool"] = fieldRule{answerToolOn, "only true"}
	workspaceRules["verify.critic"] = fieldRule{nil, "a model receives the session's tool output"}
	workspaceRules["verify.contract.protect"] = unionRule("verify.contract.protect", func(c *Config) *[]string { return &c.Verify.Contract.Protect }, "added to the protected files")
	workspaceRules["verify.contract.require"] = fieldRule{nil, "a required command is a process the harness runs"}
	workspaceRules["verify.contract.timeout_ms"] = fieldRule{nil, "a longer command time"}
}
