package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
	result := tool.Run(ctx, sess, args)
	if red := l.Recorder.redactor(); red != nil {
		result.Content = redactedText(red.Redact, result.Content)
	}
	return result, l.ManualObserve(id, call, result, time.Since(start))
}

// ManualAuthorize records the person's call and puts it to the policy. A
// refusal comes back as the result the record holds, with no tool to run.
func (l *Loop) ManualAuthorize(call, id string, args json.RawMessage) (tools.Tool, *tools.Result, error) {
	tool, found := l.Tools.Get(call)
	if !found {
		return nil, nil, fmt.Errorf("unknown tool %q", call)
	}
	refused, err := l.manualDecide(call, id, args, l.Policy.Evaluate(call, tool.Mutates(), args), Unanswered)
	if err != nil || refused != nil {
		return nil, refused, err
	}
	return tool, nil, nil
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
	decision := l.Policy.Evaluate("bash", tool.Mutates(), args)
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

func (l *Loop) manualDecide(call, id string, args json.RawMessage, decision policy.Result, answer Confirmation) (*tools.Result, error) {
	asked := answer != Unanswered && decision.Decision == policy.Ask && decision.Step == "destructive"
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
	l.record(EvActionApproved, actorFor(ByUser), approved)
	return nil, nil
}

// ManualObserve records what the person's call produced.
func (l *Loop) ManualObserve(id, call string, result tools.Result, took time.Duration) error {
	_, err := l.Recorder.Record(EvObservation, ActorTool, Untrusted, Observation{
		CallID: id, Tool: call, Content: result.Content, IsError: result.IsError,
		Truncated: result.Truncated, ExitCode: result.ExitCode,
		DurationMS: took.Milliseconds(), Sandbox: result.Tier,
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
	if l.Policy.Evaluate("bash", tool.Mutates(), args).Decision != policy.Deny {
		return nil, nil
	}
	_, refused, err := l.ManualAuthorize("bash", id, args)
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

func orEmpty(r *tools.Result) tools.Result {
	if r == nil {
		return tools.Result{}
	}
	return *r
}
