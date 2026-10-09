package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// PolicyLayer is a set of permission rules an edition adds to a session's
// policy as a layer of its own, such as the rules of the Team workspace the
// session's owner belongs to. Deny and ask rules are added to the
// configured ones and hold in every mode, as configured ones do. Allow
// rules are added only where the managed configuration leaves allow rules
// open; under one that sets the permissions they are left out and recorded
// as set aside. The layer is recorded as a policy.layer event, and every
// decision one of its rules makes names it.
type PolicyLayer struct {
	// Name is how the layer is shown and recorded, such as "workspace pay".
	Name string `json:"layer"`
	// Revision identifies the version of the rules, as the edition counts them.
	Revision string   `json:"revision,omitempty"`
	Deny     []string `json:"deny"`
	Ask      []string `json:"ask"`
	Allow    []string `json:"allow"`
}

// SessionPolicyRequest is whose session a policy layer is asked for.
type SessionPolicyRequest struct {
	Tenant string
	User   string
}

// Bounds on one layer, so an edition's store cannot make every session's
// policy unboundedly slow to evaluate.
const (
	maxLayerRules    = 500
	maxLayerRuleLen  = 1024
	maxLayerNameLen  = 80
	sessionPolicyTTL = 10 * time.Second
)

// ErrPolicyLayer refuses a session whose policy layer cannot be read or
// applied: a session never runs with fewer rules than its layer holds.
var ErrPolicyLayer = errors.New("the session's policy layer is unavailable")

// Validate checks the layer as a session would apply it: a name, and rules
// the policy engine parses (policy.ParseRule), within bounds.
func (l PolicyLayer) Validate() error {
	name := strings.TrimSpace(l.Name)
	if name == "" || len(name) > maxLayerNameLen || strings.IndexFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return fmt.Errorf("a policy layer is named in one line of 1 to %d printable characters", maxLayerNameLen)
	}
	if reservedLayer(name) {
		return fmt.Errorf("%q names one of the configuration's own layers; a policy layer needs a name of its own", name)
	}
	if n := len(l.Deny) + len(l.Ask) + len(l.Allow); n > maxLayerRules {
		return fmt.Errorf("policy layer %s has %d rules; at most %d", name, n, maxLayerRules)
	}
	for _, list := range []struct {
		name  string
		rules []string
	}{{policy.ListDeny, l.Deny}, {policy.ListAsk, l.Ask}, {policy.ListAllow, l.Allow}} {
		for _, r := range list.rules {
			if len(r) > maxLayerRuleLen {
				return fmt.Errorf("policy layer %s: a %s rule is longer than %d bytes", name, list.name, maxLayerRuleLen)
			}
			if _, err := policy.ParseRule(r); err != nil {
				return fmt.Errorf("policy layer %s: %s rule: %w", name, list.name, err)
			}
		}
	}
	return nil
}

// SHA256 is the digest of the layer's rules, in order, as the record keeps it.
func (l PolicyLayer) SHA256() string {
	b, _ := json.Marshal([3][]string{nonNil(l.Deny), nonNil(l.Ask), nonNil(l.Allow)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// LayerVerdict is what a layer's deny and ask rules say about one call.
type LayerVerdict struct {
	// Decision is "deny", "ask", or "" when no rule of the layer applies.
	Decision string
	Rule     string
	Reason   string
}

// Decide applies the layer's deny and ask rules to one call, as a session's
// policy would: a deny rule refuses, an ask rule asks, and a command that
// cannot be checked against the deny rules is refused. It is for a surface
// that runs commands outside a session, such as a sandbox's exec. A layer
// that does not validate refuses everything.
func (l PolicyLayer) Decide(tool string, args json.RawMessage) LayerVerdict {
	e := policy.New(policy.ModeDefault)
	if err := e.AddLayer(orName(l.Name), l.Deny, l.Ask, nil); err != nil {
		return LayerVerdict{Decision: string(policy.Deny), Reason: err.Error()}
	}
	res := e.Evaluate(tool, true, args)
	switch {
	case res.Decision == policy.Deny && (res.Step == "deny" || res.Step == "screen"):
		return LayerVerdict{Decision: string(policy.Deny), Rule: res.Rule, Reason: res.Reason}
	case res.Decision == policy.Ask && (res.Step == "ask" || res.Step == "screen"):
		return LayerVerdict{Decision: string(policy.Ask), Rule: res.Rule, Reason: res.Reason}
	}
	return LayerVerdict{}
}

// reservedLayer reports whether name is a layer the configuration or the
// session already shows rules under, which a policy layer may not pass as.
func reservedLayer(name string) bool {
	switch strings.ToLower(name) {
	case config.LayerDefault, config.LayerUser, config.LayerSettings, config.LayerWorkspace, config.LayerManaged,
		config.LayerFlag, config.LayerEnv, "config", "session", "managed (locked)":
		return true
	}
	return strings.HasPrefix(strings.ToLower(name), "session ")
}

func orName(n string) string {
	if strings.TrimSpace(n) == "" {
		return "policy layer"
	}
	return n
}

// appliedLayer is a layer as one session applies it.
type appliedLayer struct {
	PolicyLayer
	// SetAside are allow rules left out under a managed configuration that
	// sets the permissions.
	SetAside []string
}

// record is the policy.layer event's payload.
func (a *appliedLayer) record() map[string]any {
	return map[string]any{"layer": a.Name, "revision": a.Revision, "sha256": a.SHA256(),
		"deny": nonNil(a.Deny), "ask": nonNil(a.Ask), "allow": nonNil(a.Allow), "set_aside": nonNil(a.SetAside)}
}

// sessionLayer asks the edition for the layer of spec's owner and checks it
// against the configuration; nil when there is none. An error refuses the
// session.
func (s *Server) sessionLayer(spec StartSpec) (*appliedLayer, error) {
	if s.opts.SessionPolicy == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionPolicyTTL)
	defer cancel()
	l, err := s.opts.SessionPolicy(ctx, SessionPolicyRequest{Tenant: spec.Tenant, User: spec.User})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPolicyLayer, err)
	}
	if l == nil || len(l.Deny)+len(l.Ask)+len(l.Allow) == 0 {
		return nil, nil
	}
	if err := l.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPolicyLayer, err)
	}
	a := &appliedLayer{PolicyLayer: PolicyLayer{Name: strings.TrimSpace(l.Name), Revision: l.Revision,
		Deny: slices.Clone(l.Deny), Ask: slices.Clone(l.Ask), Allow: slices.Clone(l.Allow)}}
	if len(a.Allow) > 0 && s.opts.Config.AllowLocked() {
		a.SetAside, a.Allow = a.Allow, nil
	}
	return a, nil
}

// applyLayer adds a's rules to pol.
func applyLayer(pol *policy.Engine, a *appliedLayer) error {
	if a == nil {
		return nil
	}
	if err := pol.AddLayer(a.Name, a.Deny, a.Ask, a.Allow); err != nil {
		return fmt.Errorf("%w: %w", ErrPolicyLayer, err)
	}
	return nil
}

// recordLayer writes the session's policy.layer event, when it has a layer.
func recordLayer(rec *agent.Recorder, a *appliedLayer) error {
	if a == nil {
		return nil
	}
	if _, err := rec.Record(agent.EvPolicyLayer, agent.ActorSystem, agent.Trusted, a.record()); err != nil {
		return fmt.Errorf("record the session's policy layer: %w", err)
	}
	return nil
}

// layerChange is the policy.layer event a continued session owes its record:
// the layer now, when it differs from the last one the record names, or an
// empty one under the old name when the layer has gone. Nil when nothing changed.
func layerChange(events []agent.Event, a *appliedLayer) map[string]any {
	var last map[string]any
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == agent.EvPolicyLayer {
			_ = json.Unmarshal(events[i].Payload, &last)
			break
		}
	}
	lastSum, _ := last["sha256"].(string)
	lastName, _ := last["layer"].(string)
	switch {
	case a != nil && (a.SHA256() != lastSum || a.Name != lastName):
		return a.record()
	case a == nil && last != nil && lastSum != (PolicyLayer{}).SHA256():
		gone := (&appliedLayer{PolicyLayer: PolicyLayer{Name: lastName}}).record()
		gone["removed"] = true
		return gone
	}
	return nil
}

// permissionRow is one rule in force in a session, with the layer it came from.
type permissionRow struct {
	Layer string `json:"layer"`
	List  string `json:"list"`
	Rule  string `json:"rule"`
}

type permissionsResponse struct {
	Mode    string          `json:"mode"`
	Managed bool            `json:"managed"`
	Rules   []permissionRow `json:"rules"`
	// SetAside are rules a layer gave that the session does not apply.
	SetAside []permissionRow `json:"set_aside,omitempty"`
	Layer    *struct {
		Name     string `json:"name"`
		Revision string `json:"revision,omitempty"`
		SHA256   string `json:"sha256"`
	} `json:"policy_layer,omitempty"`
}

// sessionPermissions is the server's /permissions: the rules a running
// session is under, each with its layer, managed first since they win.
func (s *Server) sessionPermissions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	live, ok := s.session(r.Context(), id, TenantOf(r.Context()), UserOf(r.Context()))
	if !ok || live.Loop == nil || live.Loop.Policy == nil {
		WriteError(w, http.StatusNotFound, "session not running here")
		return
	}
	cfg, pol := s.opts.Config, live.Loop.Policy
	out := permissionsResponse{Mode: string(pol.Mode), Managed: cfg.Managed, Rules: []permissionRow{}}
	rank := map[string]int{config.LayerManaged: 0, config.LayerUser: 2, config.LayerSettings: 3,
		config.LayerWorkspace: 4, config.LayerFlag: 5, config.LayerEnv: 6, config.LayerDefault: 7}
	if a := live.layer; a != nil {
		rank[a.Name] = 1
		out.Layer = &struct {
			Name     string `json:"name"`
			Revision string `json:"revision,omitempty"`
			SHA256   string `json:"sha256"`
		}{a.Name, a.Revision, a.SHA256()}
		for _, rule := range a.SetAside {
			out.SetAside = append(out.SetAside, permissionRow{Layer: a.Name, List: policy.ListAllow, Rule: rule})
		}
	}
	for _, list := range []struct {
		name  string
		rules []policy.Rule
	}{{policy.ListDeny, pol.Deny}, {policy.ListAsk, pol.Ask}, {policy.ListAllow, pol.Allow}} {
		var rows []permissionRow
		for _, rule := range list.rules {
			layer := rule.Layer()
			if layer == "" {
				layer = cfg.RuleLayer(list.name, rule.String())
			}
			rows = append(rows, permissionRow{Layer: layer, List: list.name, Rule: rule.String()})
		}
		sort.SliceStable(rows, func(i, j int) bool { return rankOf(rank, rows[i].Layer) < rankOf(rank, rows[j].Layer) })
		out.Rules = append(out.Rules, rows...)
	}
	deny, ask, allow := pol.Session.SessionRules()
	for _, l := range []struct {
		name  string
		rules []string
	}{{policy.ListDeny, deny}, {policy.ListAsk, ask}, {policy.ListAllow, allow}} {
		for _, rule := range l.rules {
			out.Rules = append(out.Rules, permissionRow{Layer: "session", List: l.name, Rule: rule})
		}
	}
	live.mu.Lock()
	scopes := make([]string, 0, len(live.allowed))
	for sc := range live.allowed {
		scopes = append(scopes, sc)
	}
	live.mu.Unlock()
	sort.Strings(scopes)
	for _, sc := range scopes {
		out.Rules = append(out.Rules, permissionRow{Layer: "session (always allow)", List: policy.ListAllow, Rule: sc})
	}
	WriteJSON(w, http.StatusOK, out)
}

func rankOf(rank map[string]int, layer string) int {
	if n, ok := rank[layer]; ok {
		return n
	}
	return 8
}
