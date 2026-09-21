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
	tool, found := l.Tools.Get(call)
	if !found {
		return tools.Result{}, fmt.Errorf("unknown tool %q", call)
	}
	decision := l.Policy.Evaluate(call, tool.Mutates(), args)

	if _, err := l.Recorder.Record(EvActionRequested, ActorUser, Trusted, ActionRequested{
		CallID: id, Tool: call, Args: args, Reason: decision.Reason, Scope: decision.Scope,
	}); err != nil {
		return tools.Result{}, err
	}
	if decision.Decision == policy.Deny {
		l.record(EvActionDenied, ActorSystem, map[string]string{
			"call_id": id, "reason": decision.Reason, "step": decision.Step,
		})
		return tools.Result{Content: "Denied: " + decision.Reason, IsError: true}, nil
	}
	l.record(EvActionApproved, ActorSystem, map[string]string{
		"call_id": id, "reason": decision.Reason, "step": decision.Step, "by": "user",
	})

	start := time.Now()
	result := tool.Run(ctx, sess, args)
	_, err := l.Recorder.Record(EvObservation, ActorTool, Untrusted, Observation{
		CallID: id, Tool: call, Content: result.Content, IsError: result.IsError,
		Truncated: result.Truncated, ExitCode: result.ExitCode,
		DurationMS: time.Since(start).Milliseconds(),
	})
	return result, err
}
