package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Manual runs a tool call made by the person at the keyboard rather than by
// the model. It passes the same policy, runs the same tool in the same sandbox
// and leaves the same events as the agent's call would. The interactive
// terminal is the exception: past the call that opens it, only the sandbox
// bounds a shell, and its lines are screened and recorded best effort.
//
// An Ask is taken as answered: the person who would be asked is the caller.
// A Deny holds for them as it does for the agent.
func (l *Loop) Manual(ctx context.Context, sess *tools.Session, call string, id string, args json.RawMessage) (tools.Result, error) {
	tool, refused, err := l.ManualAuthorize(call, id, args)
	if err != nil || refused != nil {
		return orEmpty(refused), err
	}
	start := time.Now()
	result := tool.Run(l.withLaunch(ctx, id), sess, args)
	if red := l.Recorder.redactor(); red != nil {
		result.Content = redactedText(red.Redact, result.Content)
	}
	return result, l.ManualObserve(id, call, result, time.Since(start))
}

// Subject is a further argument a person's action is judged by. Action names the
// rule it is put to, such as delete for what a rename removes; empty means the action's own.
type Subject struct {
	Action string
	Args   json.RawMessage
}

// ManualAs judges and records a person's action with no tool of its own, such as an
// explorer delete, as action; the tool named run then carries it out, not judged as itself.
// also are further subjects the action is judged by, such as a rename's new name;
// a deny on any is recorded as the action's, and failing that a rule's ask.
func (l *Loop) ManualAs(ctx context.Context, sess *tools.Session, action, id string, args json.RawMessage, run string, runArgs json.RawMessage, also ...Subject) (tools.Result, error) {
	tool, found := l.Tools.Get(run)
	if !found {
		return tools.Result{}, fmt.Errorf("unknown tool %q", run)
	}
	// ctx bounds the policy hooks of every subject, so a slow hook cannot
	// hold a large batch of them past its deadline.
	decision := l.Policy.EvaluateContext(ctx, action, true, args)
	if refused := l.namesSecret(action, args); refused != "" {
		decision = policy.Result{Decision: policy.Deny, Reason: refused, Step: "deny"}
	}
	seen := make(map[string]bool, len(also))
	for _, a := range also {
		if decision.Decision == policy.Deny {
			break
		}
		name := a.Action
		if name == "" {
			name = action
		}
		key := name + "\x00" + string(a.Args)
		if seen[key] {
			continue
		}
		seen[key] = true
		if d := l.Policy.EvaluateContext(ctx, name, true, a.Args); weight(d) > weight(decision) {
			decision = d
		}
	}
	refused, err := l.manualDecide(action, id, args, decision, Unanswered)
	if err != nil || refused != nil {
		return orEmpty(refused), err
	}
	start := time.Now()
	result := tool.Run(l.withLaunch(ctx, id), sess, runArgs)
	if red := l.Recorder.redactor(); red != nil {
		result.Content = redactedText(red.Redact, result.Content)
	}
	return result, l.ManualObserve(id, action, result, time.Since(start))
}

// ManualCheck judges and records a person's action that no tool carries out,
// such as Studio removing a file the agent made; a refusal is the result.
func (l *Loop) ManualCheck(action, id string, args json.RawMessage) (*tools.Result, error) {
	decision := l.Policy.Evaluate(action, true, args)
	if why := l.pathSecretRefused(model.ToolCall{Name: action, Args: args}); why != "" {
		decision = policy.Result{Decision: policy.Deny, Reason: why, Step: "deny"}
	}
	return l.manualDecide(action, id, args, decision, Unanswered)
}

// ManualRefused records a person's action that a policy denial refused before
// ManualAs was reached, such as an explorer change to a path a write rule keeps.
func (l *Loop) ManualRefused(action, id string, args json.RawMessage, denial policy.Result) error {
	if denial.Decision != policy.Deny {
		return fmt.Errorf("%s was not denied", action)
	}
	_, err := l.manualDecide(action, id, args, denial, Unanswered)
	return err
}

// ManualAuthorize records the person's call and puts it to the policy. A
// refusal comes back as the result the record holds, with no tool to run.
func (l *Loop) ManualAuthorize(call, id string, args json.RawMessage) (tools.Tool, *tools.Result, error) {
	tool, found := l.Tools.Get(call)
	if !found {
		return nil, nil, fmt.Errorf("unknown tool %q", call)
	}
	if call == "bash" {
		_ = l.noteTrustedStartup()
	}
	decision := l.Policy.Evaluate(call, tools.MutatesCall(tool, args), args)
	if why := l.pathSecretRefused(model.ToolCall{Name: call, Args: args}); why != "" {
		decision = policy.Result{Decision: policy.Deny, Reason: why, Step: "deny"}
	}
	refused, err := l.manualDecide(call, id, args, decision, Unanswered)
	if err != nil || refused != nil {
		return nil, refused, err
	}
	return tool, nil, nil
}

// ManualAsk puts a supervisor's ask about a person's own action to the
// person, and reports whether they approved it.
type ManualAsk func(ctx context.Context, decision policy.Result) (bool, error)

// ManualAuthorizeReviewed is ManualAuthorize with the supervisor's review
// after policy, for a person's action that starts something the agent's own
// call would, such as a debug session: it may only make the decision
// stricter, and its verdict is in the record. A policy ask is taken as
// answered, since the person asked for the action; an ask the supervisor
// raised is put to the person with ask, and the action goes ahead only on
// their approval. No ask, an error or no answer refuses it.
func (l *Loop) ManualAuthorizeReviewed(ctx context.Context, call, id string, args json.RawMessage, ask ManualAsk) (tools.Tool, *tools.Result, error) {
	return l.ManualAuthorizeAsking(ctx, call, id, args, ask, "")
}

// ManualAuthorizeAsking is ManualAuthorizeReviewed that also puts the
// action to the person, with why as the reason, whenever why is set and
// policy and the supervisor would let it run: for an action the engine
// knows a workspace file can change, such as a rust-analyzer.toml that
// overrides the engine's settings. It is recorded as an ask at step
// "workspace"; no ask, an error or no answer refuses it. When the
// supervisor asks about it too, the one question it puts carries both
// reasons, and both are recorded with it.
func (l *Loop) ManualAuthorizeAsking(ctx context.Context, call, id string, args json.RawMessage, ask ManualAsk, why string) (tools.Tool, *tools.Result, error) {
	tool, found := l.Tools.Get(call)
	if !found {
		return nil, nil, fmt.Errorf("unknown tool %q", call)
	}
	if call == "bash" {
		_ = l.noteTrustedStartup()
	}
	mutates := tools.MutatesCall(tool, args)
	decision := l.Policy.Evaluate(call, mutates, args)
	if why := l.pathSecretRefused(model.ToolCall{Name: call, Args: args}); why != "" {
		decision = policy.Result{Decision: policy.Deny, Reason: why, Step: "deny"}
	}
	answer := Unanswered
	if l.Monitor != nil && decision.Decision != policy.Deny {
		decision = l.reviewed(ctx, model.ToolCall{ID: id, Name: call, Args: args}, mutates, decision)
		if decision.Decision == policy.Ask && decision.Step == "monitor" {
			// One question carries both reasons: the person answering the
			// supervisor also hears what the workspace changes.
			if why != "" {
				decision.Reason += "; and " + why
			}
			var ok bool
			err := errors.New("no one to ask")
			if ask != nil {
				ok, err = ask(ctx, decision)
			}
			switch {
			case err != nil:
				decision = policy.Result{Decision: policy.Deny, Reason: decision.Reason + "; not answered", Step: "unanswered"}
			case ok:
				answer = Confirmed
			default:
				answer = Declined
			}
		}
	}
	if why != "" && decision.Decision != policy.Deny && answer == Unanswered {
		decision = policy.Result{Decision: policy.Ask, Reason: why, Step: "workspace"}
		var ok bool
		err := errors.New("no one to ask")
		if ask != nil {
			ok, err = ask(ctx, decision)
		}
		switch {
		case err != nil:
			decision = policy.Result{Decision: policy.Deny, Reason: why + "; not answered", Step: "unanswered"}
		case ok:
			answer = Confirmed
		default:
			answer = Declined
		}
	}
	refused, err := l.manualDecide(call, id, args, decision, answer)
	if err != nil || refused != nil {
		return nil, refused, err
	}
	return tool, nil, nil
}

// ManualRunTool runs tool as the person's own call, the tool given rather
// than looked up, for an action the person takes themselves with a tool the
// agent may not have, such as reading a page they link while the agent's
// web_fetch is off. Policy, the secret checks and the supervisor judge it as
// any call: a deny refuses it, and any ask, a rule's included, is put to the
// person with ask; no ask, an error or no answer refuses it. askOf, when
// set, is the tool's own read-only ask (such as web_fetch's with no allowed
// hosts), judged as policy judges it for the agent's calls when the tool is
// on, so an allow rule that names the call answers it. A call the tool's
// Precheck refuses is refused before anyone is asked.
func (l *Loop) ManualRunTool(ctx context.Context, sess *tools.Session, tool tools.Tool, id string, args json.RawMessage, ask ManualAsk, askOf func(subject string) string) (tools.Result, error) {
	call := tool.Name()
	mutates := tools.MutatesCall(tool, args)
	pol := l.Policy
	if askOf != nil {
		pol = pol.WithReadOnlyAsk(call, askOf)
	}
	decision := pol.Evaluate(call, mutates, args)
	if why := l.pathSecretRefused(model.ToolCall{Name: call, Args: args}); why != "" {
		decision = policy.Result{Decision: policy.Deny, Reason: why, Step: "deny"}
	}
	if decision.Decision != policy.Deny {
		if err := tools.Precheck(ctx, tool, sess, args); err != nil {
			decision = policy.Result{Decision: policy.Deny, Reason: err.Error(), Step: "precheck"}
		}
	}
	if l.Monitor != nil && decision.Decision != policy.Deny {
		decision = l.reviewed(ctx, model.ToolCall{ID: id, Name: call, Args: args}, mutates, decision)
	}
	answer := Unanswered
	if decision.Decision == policy.Ask {
		ok, err := false, errors.New("no one to ask")
		if ask != nil {
			ok, err = ask(ctx, decision)
		}
		switch {
		case err != nil:
			decision = policy.Result{Decision: policy.Deny, Reason: decision.Reason + "; not answered", Step: "unanswered"}
		case ok:
			answer = Confirmed
		default:
			answer = Declined
		}
	}
	refused, err := l.manualDecide(call, id, args, decision, answer)
	if err != nil {
		return tools.Result{}, err
	}
	if refused != nil {
		return *refused, nil
	}
	start := time.Now()
	result := tool.Run(l.withLaunch(ctx, id), sess, args)
	if red := l.Recorder.redactor(); red != nil {
		result.Content = redactedText(red.Redact, result.Content)
	}
	return result, l.ManualObserve(id, call, result, time.Since(start))
}

// namesSecret applies the file-name secret check to the name an explorer
// action creates: a new folder's path, or a rename's new path.
func (l *Loop) namesSecret(action string, args json.RawMessage) string {
	var a struct {
		Path string `json:"path"`
		To   string `json:"to"`
	}
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	switch action {
	case "mkdir", "write", "edit":
		return l.pathHoldsSecret(a.Path)
	case "rename":
		return l.pathHoldsSecret(a.To)
	}
	return ""
}

// Confirmation is the person's answer to a command that always confirms.
type Confirmation int

const (
	Unanswered Confirmation = iota // no answer was sent
	Confirmed                      // the client says the person confirmed it
	Declined                       // the client says the person declined it
)

// ErrNothingToDecline answers a decline for a line that needed no
// confirmation: nothing is run or recorded.
var ErrNothingToDecline = errors.New("not run: declined")

// ManualAuthorizeTyped is ManualAuthorize for a line-by-line terminal, where a destructive
// command needs an answer; "confirmed" records the client's claim that the person gave it.
func (l *Loop) ManualAuthorizeTyped(id string, args json.RawMessage, answer Confirmation) (tool tools.Tool, refused *tools.Result, confirm string, err error) {
	tool, found := l.Tools.Get("bash")
	if !found {
		return nil, nil, "", fmt.Errorf("unknown tool %q", "bash")
	}
	_ = l.noteTrustedStartup()
	// Each line runs with the terminal as its input, so a shell it starts is judged with its start-up files.
	decision := l.Policy.EvaluateTyped("bash", tool.Mutates(), args)
	needs := decision.Decision == policy.Ask && decision.Step == "destructive"
	switch {
	case needs && answer == Unanswered:
		return nil, nil, decision.Reason, nil
	case !needs && answer == Declined && decision.Decision != policy.Deny:
		return nil, nil, "", ErrNothingToDecline
	}
	if refused, err = l.manualDecide("bash", id, args, decision, answer); err != nil || refused != nil {
		return nil, refused, "", err
	}
	return tool, nil, "", nil
}

// manualDecide records a person's action as the policy, and the person's
// answer, decided it. answer must be what the person said to the very ask
// decision names (Confirmed or Declined), or Unanswered when nothing was put
// to them: an answered ask is recorded requires_approval and confirmed, so a
// caller must never pass Confirmed for an ask the person did not see.
func (l *Loop) manualDecide(call, id string, args json.RawMessage, decision policy.Result, answer Confirmation) (*tools.Result, error) {
	asked := answer != Unanswered && decision.Decision == policy.Ask
	if _, err := l.Recorder.Record(EvActionRequested, ActorUser, Trusted, ActionRequested{
		CallID: id, Tool: call, Args: args, Reason: decision.Reason, Scope: decision.Offer(), RequiresApproval: asked,
	}); err != nil {
		return nil, err
	}
	if decision.Decision == policy.Deny {
		l.record(EvActionDenied, ActorSystem, map[string]string{
			"call_id": id, "reason": decision.Reason, "step": decision.Step, "by": ByPolicy,
		})
		return &tools.Result{Content: "Denied: " + decision.Reason, IsError: true}, nil
	}
	if answer == Declined {
		l.record(EvActionDenied, ActorUser, map[string]string{
			"call_id": id, "reason": "rejected: " + decision.Reason, "step": decision.Step, "by": "user",
		})
		return &tools.Result{Content: "Not run: not confirmed (" + decision.Reason + ")", IsError: true}, nil
	}
	approved := map[string]string{"call_id": id, "reason": decision.Reason, "step": decision.Step, "by": "user"}
	if asked && answer == Confirmed {
		approved["confirmed"] = "true"
	}
	// Not run unless its approval is on record, as for the model's calls.
	if _, err := l.Recorder.Record(EvActionApproved, actorFor(ByUser), Trusted, approved); err != nil {
		l.noteRecordErr(err)
		return nil, err
	}
	return nil, nil
}

// ManualObserve records what the person's call produced.
func (l *Loop) ManualObserve(id, call string, result tools.Result, took time.Duration) error {
	_, err := l.Recorder.Record(EvObservation, ActorTool, Untrusted, Observation{
		CallID: id, Tool: call, Content: result.Content, IsError: result.IsError,
		Truncated: result.Truncated, ExitCode: result.ExitCode,
		DurationMS: took.Milliseconds(), Sandbox: result.Tier, SandboxNote: result.SandboxNote,
	})
	return err
}

// ManualScreen judges a line entered at an interactive terminal before the
// shell is given it. A refusal is recorded as the person's denied bash call
// with its result; an allowed line is left to ManualTerminalInput.
func (l *Loop) ManualScreen(id, line string) (*tools.Result, error) {
	tool, found := l.Tools.Get("bash")
	if !found {
		return nil, fmt.Errorf("unknown tool %q", "bash")
	}
	args, _ := json.Marshal(map[string]string{"command": line, "description": "entered in the interactive terminal"})
	_ = l.noteTrustedStartup()
	// The line's own judgement is the one recorded: judged again as a whole
	// command, a shell it starts would not be read with its start-up files.
	decision := l.Policy.EvaluateLine("bash", tool.Mutates(), args)
	if decision.Decision != policy.Deny {
		return nil, nil
	}
	refused, err := l.manualDecide("bash", id, args, decision, Unanswered)
	if err != nil || refused == nil {
		return refused, err
	}
	return refused, l.ManualObserve(id, "bash", *refused, 0)
}

// ManualTerminalInput records a line entered at an interactive terminal.
func (l *Loop) ManualTerminalInput(in TerminalInput) error {
	_, err := l.Recorder.Record(EvTerminalInput, ActorUser, Trusted, in)
	return err
}

// weight ranks a decision for ManualAs: a deny, then an ask a rule or hook
// made, then an ask the mode made, then an allow.
func weight(d policy.Result) int {
	switch {
	case d.Decision == policy.Deny:
		return 3
	case d.Decision == policy.Ask && d.Step != "mode" && d.Step != "default":
		return 2
	case d.Decision == policy.Ask:
		return 1
	}
	return 0
}

func orEmpty(r *tools.Result) tools.Result {
	if r == nil {
		return tools.Result{}
	}
	return *r
}
