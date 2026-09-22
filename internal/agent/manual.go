package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Manual runs a tool call made by the person at the keyboard rather than by
// the model. It passes the same policy, runs the same tool in the same sandbox
// and leaves the same events, so a person in the workbench holds no power the
// agent lacks and nothing they do is missing from the record.
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
	return result, l.ManualObserve(id, call, result, time.Since(start))
}

// ManualAuthorize records the person's call and puts it to the policy. A
// refusal comes back as the result the record holds, with no tool to run.
func (l *Loop) ManualAuthorize(call, id string, args json.RawMessage) (tools.Tool, *tools.Result, error) {
	tool, found := l.Tools.Get(call)
	if !found {
		return nil, nil, fmt.Errorf("unknown tool %q", call)
	}
	decision := l.Policy.Evaluate(call, tool.Mutates(), args)

	if _, err := l.Recorder.Record(EvActionRequested, ActorUser, Trusted, ActionRequested{
		CallID: id, Tool: call, Args: args, Reason: decision.Reason, Scope: decision.Scope,
	}); err != nil {
		return nil, nil, err
	}
	if decision.Decision == policy.Deny {
		l.record(EvActionDenied, ActorSystem, map[string]string{
			"call_id": id, "reason": decision.Reason, "step": decision.Step,
		})
		return nil, &tools.Result{Content: "Denied: " + decision.Reason, IsError: true}, nil
	}
	l.record(EvActionApproved, ActorSystem, map[string]string{
		"call_id": id, "reason": decision.Reason, "step": decision.Step, "by": "user",
	})
	return tool, nil, nil
}

// ManualObserve records what the person's call produced.
func (l *Loop) ManualObserve(id, call string, result tools.Result, took time.Duration) error {
	_, err := l.Recorder.Record(EvObservation, ActorTool, Untrusted, Observation{
		CallID: id, Tool: call, Content: result.Content, IsError: result.IsError,
		Truncated: result.Truncated, ExitCode: result.ExitCode,
		DurationMS: took.Milliseconds(),
	})
	return err
}

func orEmpty(r *tools.Result) tools.Result {
	if r == nil {
		return tools.Result{}
	}
	return *r
}
