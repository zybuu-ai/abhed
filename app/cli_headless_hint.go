package app

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A -p run has no one to ask, so a call that needed an answer is refused.
// Each refusal says what would allow it: the exact flag, checked against the
// policy and the configuration before it is offered, or that no flag can,
// and whose rule it is. It only explains; it grants nothing.

// permissionDenial is one refused call, as the -p result reports it.
type permissionDenial struct {
	Tool   string `json:"tool"`
	CallID string `json:"call_id"`
	Step   string `json:"step,omitempty"`
	Source string `json:"source,omitempty"`
	Rule   string `json:"rule,omitempty"`
	Reason string `json:"reason"`
	// Flag is the flag that would allow the call on a rerun, "" when none would.
	Flag string `json:"flag,omitempty"`
	// Hint says what to do, in words: the flag, or why there is none.
	Hint string `json:"hint"`
}

// denialHints follows a -p run's record and explains each refused call.
type denialHints struct {
	pol      *policy.Engine
	cfg      config.Config
	registry *tools.Registry
	asked    map[string]agent.ActionRequested
	denials  []permissionDenial
}

func newDenialHints(pol *policy.Engine, cfg config.Config, registry *tools.Registry) *denialHints {
	return &denialHints{pol: pol, cfg: cfg, registry: registry, asked: map[string]agent.ActionRequested{}}
}

// observe takes one event; for a refusal it returns the explanation.
func (h *denialHints) observe(ev agent.Event) (permissionDenial, bool) {
	switch ev.Type {
	case agent.EvActionRequested:
		var a agent.ActionRequested
		if json.Unmarshal(ev.Payload, &a) == nil && a.CallID != "" {
			h.asked[a.CallID] = a
		}
	case agent.EvActionDenied:
		var d map[string]string
		if json.Unmarshal(ev.Payload, &d) != nil {
			return permissionDenial{}, false
		}
		a, ok := h.asked[d["call_id"]]
		if !ok {
			return permissionDenial{}, false
		}
		out := permissionDenial{Tool: a.Tool, CallID: a.CallID, Step: d["step"], Source: d["source"], Rule: d["rule"], Reason: d["reason"]}
		out.Flag, out.Hint = h.hint(a, d)
		h.denials = append(h.denials, out)
		return out, true
	}
	return permissionDenial{}, false
}

// hint is the flag that would allow a refused call and what to say of it.
func (h *denialHints) hint(a agent.ActionRequested, d map[string]string) (flag, hint string) {
	step, source, rule := d["step"], d["source"], d["rule"]
	switch {
	case step == "deny" && rule != "":
		return "", denyHint(source, rule, h.where("deny", rule))
	case source == "hook":
		return "", "refused by a policy hook; no flag lifts it"
	case step == "monitor":
		return "", "refused by the supervisor; no flag lifts it"
	case step == "destructive":
		return "", "this always asks for confirmation; run it interactively"
	case step == "ask" && rule != "":
		if source == config.LayerOrg || source == config.LayerManaged {
			return "", "asked by " + orgWords(source) + " rule " + rule + " on every call, and no one can answer in -p; ask your admin, or run it interactively"
		}
		owner := "the ask rule "
		switch source {
		case config.LayerUser, config.LayerWorkspace, config.LayerSettings, config.LayerFlag, "session":
			owner = "your ask rule "
		case config.LayerDefault:
			owner = "Abhed's built-in ask rule "
		}
		s := owner + rule
		if where := h.where("ask", rule); where != "" && source != config.LayerDefault {
			s += " (" + where + ")"
		}
		return "", s + " asks on every call, and no flag lifts it; run it interactively"
	case d["by"] != agent.ByHeadless && step != "mode":
		return "", ""
	}
	if f := h.allowingFlag(a); f != "" {
		return f, "rerun with " + f + " to allow it"
	}
	if h.cfg.AllowLocked() {
		return "", "the managed configuration sets the permissions, so no flag can allow it; ask your admin, or run it interactively"
	}
	return "", "no flag allows exactly this in -p; run it interactively"
}

// denyHint says whose deny rule refused a call. A deny rule holds in every
// mode and no flag lifts one, so none is suggested.
func denyHint(source, rule, where string) string {
	switch source {
	case config.LayerOrg, config.LayerManaged:
		return "denied by " + orgWords(source) + "; ask your admin"
	case config.LayerUser, config.LayerWorkspace, config.LayerSettings, config.LayerFlag, config.LayerDefault, "session", "config", "":
		if where == "" {
			where = "the rules"
		}
		return "no flag lifts a deny rule; remove " + rule + " from " + where + " to allow this"
	}
	return "denied by the " + source + " policy layer; ask your admin"
}

func orgWords(source string) string {
	if source == config.LayerOrg {
		return "org policy"
	}
	return "the managed configuration"
}

// where is the configuration a rule came from, in words.
func (h *denialHints) where(list, rule string) string {
	_, where := h.cfg.RuleSource(list, rule)
	return where
}

// allowingFlag is the first flag that would allow the call on a rerun: one
// the configuration accepts and under which the policy, rules only, allows
// the call. "" when none does.
func (h *denialHints) allowingFlag(a agent.ActionRequested) string {
	tool, ok := h.registry.Get(a.Tool)
	if !ok || h.pol == nil {
		return ""
	}
	mutates := tools.MutatesCall(tool, a.Args)
	allow := h.pol.AllowRuleFor(a.Tool, mutates, a.Args)
	type candidate struct {
		mode  string
		allow string
	}
	var cands []candidate
	if a.Tool == "edit" || a.Tool == "write" {
		cands = append(cands, candidate{mode: string(policy.ModeAcceptEdits)})
	}
	if allow != "" {
		cands = append(cands, candidate{allow: allow})
		if h.pol.Mode == policy.ModePlan {
			cands = append(cands, candidate{mode: string(policy.ModeDefault), allow: allow})
		}
	}
	for _, c := range cands {
		o := config.Overrides{Mode: c.mode}
		if c.allow != "" {
			o.Allow = []string{c.allow}
		}
		if _, err := h.cfg.Apply(o); err != nil {
			continue // the configuration refuses the flag itself
		}
		sim := h.pol.WithoutHooks()
		if c.mode != "" {
			sim.Mode = policy.Mode(c.mode)
		}
		if c.allow != "" {
			r, err := policy.ParseRule(c.allow)
			if err != nil {
				continue
			}
			sim.Allow = append(slices.Clone(sim.Allow), r)
		}
		if sim.Evaluate(a.Tool, mutates, a.Args).Decision != policy.Allow {
			continue
		}
		var parts []string
		if c.mode != "" {
			parts = append(parts, "-mode "+c.mode)
		}
		if c.allow != "" {
			parts = append(parts, "-allow "+shellQuote(c.allow))
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// String is the line a person reads on stderr.
func (d permissionDenial) String() string {
	s := fmt.Sprintf("denied %s: %s", d.Tool, d.Reason)
	if d.Hint != "" {
		s += " (" + d.Hint + ")"
	}
	return s
}
