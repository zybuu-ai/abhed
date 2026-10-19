package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// EvPlanStep is a call that ran as a step of the approved runbook; see PlanStep.
const EvPlanStep EventType = "plan.step"

// ByPlan marks a call the person approved in advance, as a step of the plan
// they accepted: the plan's approval took the place of the ask.
const ByPlan = "plan"

// ViaOperator marks a runbook bound by the operator's own setting (the -runbook
// flag, or an embedder's Options.Runbook) rather than a person's answer.
const ViaOperator = "operator"

// maxRunbookSteps bounds one runbook, so a card can list every step.
const maxRunbookSteps = 200

// runbookTools are the tools a runbook step may call.
var runbookTools = map[string]bool{"ssh": true, "k8s_apply": true, "bash": true, "edit": true, "write": true}

// Runbook is a plan's runbook as recorded: each step canonicalised as the
// loop would see the call, and hashed, with the hash of the whole plan.
type Runbook struct {
	// Hash is the sha256 of the plan's text and every step's hash, in order.
	Hash  string        `json:"hash"`
	Steps []RunbookStep `json:"steps"`
}

// RunbookStep is one canonical step of a Runbook.
type RunbookStep struct {
	Index int    `json:"index"` // 1-based
	Tool  string `json:"tool"`
	// Target is where the step runs, as the person reads it: host, cluster/namespace or path.
	Target string          `json:"target,omitempty"`
	Args   json.RawMessage `json:"args"`
	Hash   string          `json:"hash"`
	Note   string          `json:"note,omitempty"`
	// Destructive says why the step cannot be undone; "" when it can.
	Destructive string `json:"destructive,omitempty"`
}

// PlanStep is the payload of plan.step: a call that ran as a runbook step.
type PlanStep struct {
	CallID   string `json:"call_id"`
	PlanHash string `json:"plan_hash"`
	Step     int    `json:"step"` // 1-based
	Of       int    `json:"of"`
	StepHash string `json:"step_hash"`
	Tool     string `json:"tool"`
	Target   string `json:"target,omitempty"`
	// By is plan when the plan's approval stood in for the ask, else who let it through.
	By string `json:"by"`
}

// boundPlan is a runbook the person approved, held against the calls that follow.
type boundPlan struct {
	rb   Runbook
	done []bool
}

// next is the index of the first step not yet run, or -1 when all have.
func (b *boundPlan) next() int {
	for i, d := range b.done {
		if !d {
			return i
		}
	}
	return -1
}

// find is the first step not yet run that the call matches exactly, or -1.
func (b *boundPlan) find(tool string, args json.RawMessage) int {
	for i, s := range b.rb.Steps {
		if !b.done[i] && s.Tool == tool && bytes.Equal(s.Args, args) {
			return i
		}
	}
	return -1
}

// planHit is a call matched to a step of the bound runbook.
type planHit struct {
	plan  *boundPlan
	index int
	// replaced: the plan's approval stood in for the ask.
	replaced bool
}

// prepareRunbook canonicalises and hashes a proposed runbook, refusing a step
// that names an unknown tool, cannot be read, or that policy would refuse.
func (l *Loop) prepareRunbook(text string, steps []tools.RunbookStep) (*Runbook, error) {
	if len(steps) == 0 {
		return nil, nil
	}
	if len(steps) > maxRunbookSteps {
		return nil, fmt.Errorf("the runbook has %d steps; at most %d are allowed", len(steps), maxRunbookSteps)
	}
	rb := &Runbook{}
	sum := sha256.New()
	writeField(sum, "abhed-plan-v1")
	writeField(sum, text)
	for i, s := range steps {
		st, err := l.prepareStep(s)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i+1, err)
		}
		st.Index = i + 1
		rb.Steps = append(rb.Steps, st)
		writeField(sum, st.Hash)
	}
	rb.Hash = hex.EncodeToString(sum.Sum(nil))
	return rb, nil
}

func writeField(h interface{ Write([]byte) (int, error) }, s string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(s)) + ":" + s + ";"))
}

// prepareStep turns one proposed step into the canonical call it stands for.
func (l *Loop) prepareStep(s tools.RunbookStep) (RunbookStep, error) {
	name := strings.TrimSpace(s.Tool)
	if !runbookTools[name] {
		return RunbookStep{}, fmt.Errorf("a runbook step calls ssh, k8s_apply, bash, edit or write, not %q", s.Tool)
	}
	tool, ok := l.Tools.Get(name)
	if !ok {
		return RunbookStep{}, fmt.Errorf("the tool %s is not available in this session", name)
	}
	raw, err := stepArgs(name, s)
	if err != nil {
		return RunbookStep{}, err
	}
	canon, err := l.canonicalCall(tool, raw)
	if err != nil {
		return RunbookStep{}, err
	}
	st := RunbookStep{Tool: name, Target: stepTarget(name, canon), Args: canon, Note: strings.TrimSpace(s.Note)}
	sum := sha256.New()
	writeField(sum, name)
	writeField(sum, string(canon))
	st.Hash = hex.EncodeToString(sum.Sum(nil))

	// Judged as the call will be outside plan mode, without the hooks, which
	// still judge the call itself when it is made: a refusal now is final.
	if l.Policy != nil {
		dry := l.Policy.WithoutHooks()
		dry.Mode = policy.ModeDefault
		d := dry.Evaluate(name, tools.MutatesCall(tool, canon), canon)
		switch {
		case d.Decision == policy.Deny:
			return RunbookStep{}, fmt.Errorf("the policy refuses it (%s)", d.Reason)
		case d.Decision == policy.Ask && d.Step == "destructive":
			st.Destructive = strings.TrimSuffix(d.Reason, " — always requires confirmation")
		}
		if st.Destructive == "" {
			st.Destructive = remoteDestructive(name, canon, dry.GitExtensions)
		}
	}
	return st, nil
}

// remoteDestructive names a remote step with no undo the policy's own
// destructive step does not read: a destructive command over ssh, or a
// Kubernetes delete.
func remoteDestructive(tool string, canon json.RawMessage, git map[string]bool) string {
	m, err := tools.DecodeArgs(canon)
	if err != nil {
		return ""
	}
	switch tool {
	case "ssh":
		if what, ok := tools.CanonicalCommand(argStr(m, "command")).DestructiveWith(git); ok {
			return what + " on " + argStr(m, "host")
		}
	case "k8s_apply":
		if argStr(m, "action") == "delete" {
			return "deletes " + argStr(m, "resource") + " " + argStr(m, "name") + " from the cluster"
		}
	}
	return ""
}

// stepArgs builds the call's arguments from a step.
func stepArgs(tool string, s tools.RunbookStep) (json.RawMessage, error) {
	args := map[string]any{}
	if t := bytes.TrimSpace(s.Args); len(t) > 0 && string(t) != "null" {
		if err := json.Unmarshal(t, &args); err != nil {
			return nil, fmt.Errorf("args is not a JSON object: %v", err)
		}
	}
	set := func(key, v string) error {
		if v == "" {
			return nil
		}
		if old, ok := args[key]; ok && old != v {
			return fmt.Errorf("%s is given twice, differently", key)
		}
		args[key] = v
		return nil
	}
	target := strings.TrimSpace(s.Target)
	var err error
	switch tool {
	case "ssh":
		err = errors.Join(set("host", target), set("command", s.Command))
		if argStr(args, "host") == "" {
			return nil, errors.New("an ssh step names its host in target")
		}
	case "k8s_apply":
		if target != "" {
			cluster, ns, _ := strings.Cut(target, "/")
			err = errors.Join(set("cluster", cluster), set("namespace", ns))
		}
		err = errors.Join(err, set("manifest", s.Manifest))
		if _, ok := args["action"]; !ok && s.Manifest != "" {
			args["action"] = "apply"
		}
		// The cluster is named, so the step cannot come to mean another one.
		if argStr(args, "cluster") == "" && argStr(args, "context") == "" {
			return nil, errors.New("a k8s_apply step names its cluster in target (cluster/namespace)")
		}
	case "bash":
		err = set("command", s.Command)
		if target != "" {
			return nil, errors.New("a bash step runs in the session's sandbox and takes no target")
		}
	case "edit", "write":
		err = set("path", target)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(args)
}

// stepTarget is where a canonical step runs, as a person reads it.
func stepTarget(tool string, canon json.RawMessage) string {
	m, err := tools.DecodeArgs(canon)
	if err != nil {
		return ""
	}
	switch tool {
	case "ssh":
		return argStr(m, "host")
	case "k8s_apply":
		c := argStr(m, "cluster")
		if c == "" {
			c = argStr(m, "context")
		}
		return strings.Trim(c+"/"+argStr(m, "namespace"), "/")
	case "edit", "write":
		return argStr(m, "path")
	}
	return ""
}

func argStr(m map[string]any, k string) string { v, _ := m[k].(string); return v }

// canonicalCall is the form authorize gives a call before policy judges it:
// canonical arguments, credentials withheld, its destination resolved.
func (l *Loop) canonicalCall(tool tools.Tool, raw json.RawMessage) (json.RawMessage, error) {
	canon, _, err := tools.CanonicalArgs(tool, raw)
	if err != nil {
		return nil, err
	}
	canon = tools.WithholdSecretValues(tool, canon)
	if r, ok := tool.(tools.ArgResolver); ok {
		out, _, err := r.ResolveArgs(l.Session, canon)
		if err != nil {
			return nil, err
		}
		if out != nil {
			if again, _, err := tools.CanonicalArgs(tool, out); err == nil {
				canon = again
			}
		}
	}
	return canon, nil
}

// BindPlan holds the session to the runbook of the plan just proposed with
// hash, once the person has accepted it. A plan without a runbook binds
// nothing, and returns nil.
func (l *Loop) BindPlan(hash string) error {
	l.plans.mu.Lock()
	defer l.plans.mu.Unlock()
	rb := l.plans.offered
	if rb == nil {
		return nil
	}
	if rb.Hash != hash {
		return errors.New("the plan accepted is not the one proposed")
	}
	l.plans.offered = nil
	l.plans.bound = &boundPlan{rb: *rb, done: make([]bool, len(rb.Steps))}
	return nil
}

// BindRunbook binds a runbook the operator supplied, with no person asked:
// the operator's setting is the approval. It is recorded as plan.proposed
// and plan.decided, via operator.
func (l *Loop) BindRunbook(text string, steps []tools.RunbookStep) (Runbook, error) {
	if len(steps) == 0 {
		return Runbook{}, errors.New("the runbook has no steps")
	}
	rb, err := l.prepareRunbook(text, steps)
	if err != nil {
		return Runbook{}, err
	}
	if _, err := l.Recorder.Record(EvPlanProposed, ActorUser, Trusted, PlanProposed{Text: text, Runbook: rb}); err != nil {
		return Runbook{}, err
	}
	if _, err := l.Recorder.Record(EvPlanDecided, ActorUser, Trusted, PlanDecided{
		Decision: PlanAccepted, Via: ViaOperator, PlanHash: rb.Hash, Runbook: rb,
	}); err != nil {
		return Runbook{}, err
	}
	l.plans.mu.Lock()
	l.plans.offered = nil
	l.plans.bound = &boundPlan{rb: *rb, done: make([]bool, len(rb.Steps))}
	l.plans.mu.Unlock()
	return *rb, nil
}

// UnbindPlan lets go of the bound runbook: every call is judged by policy alone again.
func (l *Loop) UnbindPlan() {
	l.plans.mu.Lock()
	l.plans.bound = nil
	l.plans.mu.Unlock()
}

// BoundRunbook reports the bound runbook, the steps already run, and
// whether one is bound.
func (l *Loop) BoundRunbook() (Runbook, []bool, bool) {
	l.plans.mu.Lock()
	defer l.plans.mu.Unlock()
	if l.plans.bound == nil {
		return Runbook{}, nil, false
	}
	return l.plans.bound.rb, append([]bool(nil), l.plans.bound.done...), true
}

// planBoundKey marks the context of a subagent spawned under a bound runbook.
type planBoundKey struct{}

// markPlanBound carries a bound runbook down to the subagents a call spawns.
func (l *Loop) markPlanBound(ctx context.Context) context.Context {
	l.plans.mu.Lock()
	bound := l.plans.bound != nil
	l.plans.mu.Unlock()
	if !bound || ctx.Value(planBoundKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, planBoundKey{}, true)
}

// planReplaces are the steps whose ask an approved runbook step stands in
// for. A handover, a hook's or the supervisor's ask still puts the call to a person.
var planReplaces = map[string]bool{"default": true, "mode": true, "ask": true, "destructive": true, "screen": true}

// planBind holds a call to the bound runbook. A call that is the next step,
// exactly, runs on the plan's approval in place of an ask; a mutating call
// that is not, or comes out of order, asks with "not in the approved plan"
// even where the policy alone would allow it. A deny is never touched.
func (l *Loop) planBind(ctx context.Context, tool tools.Tool, call model.ToolCall, d policy.Result) (policy.Result, *planHit) {
	if d.Decision == policy.Deny {
		return d, nil
	}
	mutates := tools.MutatesCall(tool, call.Args)
	l.plans.mu.Lock()
	b := l.plans.bound
	idx, next := -1, -1
	if b != nil {
		idx, next = b.find(call.Name, call.Args), b.next()
	}
	l.plans.mu.Unlock()
	if b == nil {
		if mutates && ctx.Value(planBoundKey{}) != nil {
			return notInPlan(d, "a subagent's call is never a step of the approved plan"), nil
		}
		return d, nil
	}
	of := len(b.rb.Steps)
	if idx < 0 {
		if !mutates {
			return d, nil
		}
		why := "the plan's steps have all run"
		if next >= 0 {
			why = fmt.Sprintf("step %d of %d is next", next+1, of)
		}
		return notInPlan(d, why), nil
	}
	hit := &planHit{plan: b, index: idx}
	label := fmt.Sprintf("step %d of %d in the approved plan %s", idx+1, of, shortHash(b.rb.Hash))
	if idx != next {
		d = notInPlan(d, fmt.Sprintf("it is step %d, out of order: step %d is next", idx+1, next+1))
		return d, hit
	}
	if d.Decision == policy.Allow {
		d.Reason = strings.TrimPrefix(d.Reason+"; "+label, "; ")
		return d, hit
	}
	if !planReplaces[d.Step] {
		d.Reason = label + ", asked again: " + d.Reason
		return d, hit
	}
	// The plan's approval answers the ask; a hook on asks may still refuse it.
	if l.Hooks != nil {
		if why := l.Hooks.PermissionRequested(ctx, l.Policy, l.sessionID(), call.Name, call.Args, label); why != "" {
			return policy.Result{Decision: policy.Deny, Reason: why, Step: "hook"}, nil
		}
	}
	hit.replaced = true
	return policy.Result{Decision: policy.Allow, Step: "plan", Reason: label + " (approved with the plan; was: " + d.Reason + ")"}, hit
}

// notInPlan makes a call ask, saying it is not in the approved plan; it
// offers no "always allow", so nothing remembered lets it through unasked.
func notInPlan(d policy.Result, why string) policy.Result {
	reason := "not in the approved plan (" + why + ")"
	if d.Decision == policy.Allow {
		return policy.Result{Decision: policy.Ask, Step: "plan", Reason: reason + "; the policy alone would allow it: " + d.Reason}
	}
	d.Reason = reason + "; " + d.Reason
	d.Scope, d.Suggest, d.Guard = "", "", ""
	return d
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// planClaim marks a matched step run as its call is approved, and adds the
// plan to the approval's record. A step another call took first is refused.
func (l *Loop) planClaim(call model.ToolCall, hit *planHit, approved map[string]string) (bool, tools.Result) {
	if hit == nil {
		return true, tools.Result{}
	}
	l.plans.mu.Lock()
	ok := l.plans.bound == hit.plan && !hit.plan.done[hit.index]
	if ok {
		hit.plan.done[hit.index] = true
	}
	l.plans.mu.Unlock()
	if !ok {
		why := fmt.Sprintf("step %d of the approved plan has already run, or the plan was let go", hit.index+1)
		l.record(EvActionDenied, ActorSystem, map[string]string{
			"call_id": call.ID, "reason": why, "step": "plan", "by": BySystem,
		})
		return false, tools.Result{Content: "Denied: " + why + ".", IsError: true}
	}
	if hit.replaced {
		approved["by"] = ByPlan
	}
	approved["plan_hash"] = hit.plan.rb.Hash
	approved["plan_step"] = strconv.Itoa(hit.index + 1)
	return true, tools.Result{}
}

// recordPlanStep records the call as the runbook step it is.
func (l *Loop) recordPlanStep(call model.ToolCall, hit *planHit, by string) {
	if hit == nil {
		return
	}
	s := hit.plan.rb.Steps[hit.index]
	l.record(EvPlanStep, actorFor(by), PlanStep{
		CallID: call.ID, PlanHash: hit.plan.rb.Hash, Step: s.Index, Of: len(hit.plan.rb.Steps),
		StepHash: s.Hash, Tool: s.Tool, Target: s.Target, By: by,
	})
}

// RunbookPrompt is the message an accepted runbook goes on with: each step's
// exact call, so the model makes it as approved.
func RunbookPrompt(rb Runbook) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The runbook (plan %s) is approved. Make each step's call with exactly these arguments, in order; "+
		"any other call, or a changed argument, asks the user again.\n", shortHash(rb.Hash))
	for _, s := range rb.Steps {
		fmt.Fprintf(&b, "%d. %s %s\n", s.Index, s.Tool, s.Args)
	}
	return b.String()
}
