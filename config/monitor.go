package config

import (
	"fmt"
	"net/url"
	"slices"
)

// MonitorConfig governs the supervisor, which reviews a call after policy
// and before it runs, and may only make the decision stricter.
type MonitorConfig struct {
	// Enabled is on by default. The managed configuration may force it on;
	// a workspace may only turn it on.
	Enabled bool `json:"enabled"`
	// Judge is "rules" (the default), the deterministic judge, or "model",
	// which asks a model behind Endpoint after the rules.
	Judge string `json:"judge,omitempty"`
	// Endpoint is an OpenAI-compatible base URL, such as http://127.0.0.1:8000/v1.
	Endpoint string `json:"endpoint,omitempty"`
	Model    string `json:"model,omitempty"`
	// APIKeyEnv names the variable holding the endpoint's key, if it needs one.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// TimeoutMS bounds one judgement; zero means two seconds.
	TimeoutMS int `json:"timeout_ms,omitempty"`
	// OnUnavailable is what a call gets when the judge cannot answer: "ask"
	// (the default) or "deny". Where nobody can be asked it is always deny.
	OnUnavailable string `json:"on_unavailable,omitempty"`
	// MaxPerTurn bounds the calls put to the judge in one turn; zero is no bound.
	MaxPerTurn int `json:"max_per_turn,omitempty"`
	// Honeytokens are planted values no honest call carries; a call holding
	// one is denied.
	Honeytokens []string `json:"honeytokens,omitempty"`
}

// JudgeName is what the supervisor runs: rules, model, or off.
func (m MonitorConfig) JudgeName() string {
	switch {
	case !m.Enabled:
		return "off"
	case m.Judge == "model":
		return "model"
	}
	return "rules"
}

func (m MonitorConfig) validate() error {
	switch m.Judge {
	case "", "rules", "model":
	default:
		return fmt.Errorf("monitor.judge is %q; use rules or model", m.Judge)
	}
	switch m.OnUnavailable {
	case "", "ask", "deny":
	default:
		return fmt.Errorf("monitor.on_unavailable is %q; use ask or deny", m.OnUnavailable)
	}
	if m.TimeoutMS < 0 || m.MaxPerTurn < 0 {
		return fmt.Errorf("monitor.timeout_ms and monitor.max_per_turn may not be negative")
	}
	if m.Endpoint != "" {
		u, err := url.Parse(m.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("monitor.endpoint %q is not an http(s) URL", m.Endpoint)
		}
		if u.User != nil {
			return fmt.Errorf("monitor.endpoint may not hold a user or password; name the key's variable in monitor.api_key_env")
		}
	}
	if m.Enabled && m.Judge == "model" && (m.Endpoint == "" || m.Model == "") {
		return fmt.Errorf("monitor.judge model needs monitor.endpoint and monitor.model")
	}
	return nil
}

// monitorBelow keeps what the layers under the managed file set where it is
// stricter than the managed value: the supervisor turned on, deny when the
// judge cannot answer, more calls reviewed, and every honeytoken.
func monitorBelow(c *Config, below MonitorConfig, belowSet func(string) bool) {
	if !c.ManagedSets("monitor") {
		return
	}
	m := &c.Monitor
	// A managed monitor section that leaves enabled out keeps it on: an
	// organisation that tunes the supervisor relies on it running.
	c.monitorLocked = !c.ManagedSets("monitor.enabled") || m.Enabled
	if c.monitorLocked || below.Enabled && belowSet("monitor.enabled") {
		m.Enabled = true
	}
	if below.OnUnavailable == "deny" {
		m.OnUnavailable = "deny"
	}
	if belowSet("monitor.max_per_turn") && m.MaxPerTurn > 0 && (below.MaxPerTurn == 0 || below.MaxPerTurn > m.MaxPerTurn) {
		m.MaxPerTurn = below.MaxPerTurn
	}
	for _, t := range below.Honeytokens {
		if !slices.Contains(m.Honeytokens, t) {
			m.Honeytokens = append(m.Honeytokens, t)
		}
	}
}

// applySupervisor is Overrides.Supervisor: off, rules or model.
func (c Config) applySupervisor(v string) (Config, error) {
	switch v {
	case "off":
		if c.monitorLocked {
			return c, refuse("monitor.enabled", v, "the managed configuration keeps the supervisor on")
		}
		c.Monitor.Enabled = false
	case "rules", "model":
		if c.ManagedSets("monitor.judge") && orRules(c.Monitor.Judge) != v {
			return c, refuse("monitor.judge", v, fmt.Sprintf("the managed configuration chooses the %s judge", orRules(c.Monitor.Judge)))
		}
		c.Monitor.Enabled, c.Monitor.Judge = true, v
	default:
		return c, fmt.Errorf("unknown supervisor %q: want off, rules or model", v)
	}
	return c, c.Monitor.validate()
}

func orRules(j string) string {
	if j == "" {
		return "rules"
	}
	return j
}

// workspace rules for the monitor section: a workspace may only tighten.
func stricterUnavailable(dst, ws *Config) bool {
	if ws.Monitor.OnUnavailable != "deny" {
		return false
	}
	dst.Monitor.OnUnavailable = "deny"
	return true
}

func moreReviews(dst, ws *Config) bool {
	v, cur := ws.Monitor.MaxPerTurn, dst.Monitor.MaxPerTurn
	if cur == 0 || v < 0 || v != 0 && v <= cur {
		return false
	}
	dst.Monitor.MaxPerTurn = v
	return true
}
