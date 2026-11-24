package agent

import (
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// firstUseAsk puts the first call to something the person's own machine set
// up (a kubeconfig context, an ~/.ssh/config host) to the person, once per
// session, unless they kept a grant for it. A deny stays a deny; an allow
// rule or bypass mode answers it as they answer any ask. A call that asks
// anyway is asked once, with the first use named in its reason: its
// "always" keeps the first-use grant, never the call's own scope.
func (l *Loop) firstUseAsk(tool tools.Tool, call model.ToolCall, d policy.Result) (policy.Result, *tools.FirstUse) {
	fu, ok := tool.(tools.FirstUser)
	if !ok || d.Decision == policy.Deny {
		return d, nil
	}
	g, need := fu.FirstUse(l.Session, call.Args)
	if !need || tools.FirstUsed(l.Session, g.Kind, g.Name, g.Identity) {
		return d, nil
	}
	if d.Decision == policy.Allow && (d.Step == "allow" || l.Policy.Mode == policy.ModeBypass) {
		return d, nil
	}
	keeps := g.What
	if g.Scope == "" {
		keeps = "" // nothing is kept where the tool keeps nothing
	}
	if d.Decision == policy.Allow {
		return policy.Result{Decision: policy.Ask, Step: "default", Source: "mode",
			Reason: g.Reason, Scope: g.Scope, Keeps: keeps}, &g
	}
	d.Reason = g.Reason + "; " + d.Reason
	// Only the default ask offers a scope; there the first use's replaces
	// the call's, so "always" keeps the cluster or host and never this call.
	if d.Step == "default" {
		d.Scope, d.Keeps, d.Suggest, d.Guard = g.Scope, keeps, "", ""
	}
	return d, &g
}

// firstUseApproved notes an approved first use for the session and, when the
// person chose always, keeps it for them. A grant that cannot be kept holds
// for this session alone, and the record says so.
func (l *Loop) firstUseApproved(tool tools.Tool, g tools.FirstUse, keep bool, approvedBy map[string]string) {
	tools.MarkFirstUsed(l.Session, g)
	fu, ok := tool.(tools.FirstUser)
	if !keep || !ok {
		return
	}
	if err := fu.KeepFirstUse(g); err != nil {
		approvedBy["first_use"] = "kept for this session only: " + err.Error()
		return
	}
	approvedBy["first_use"] = "kept: " + g.What
}
