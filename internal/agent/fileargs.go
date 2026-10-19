package agent

import (
	"encoding/json"
	"fmt"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// filesJudged judges each workspace file a call reads or writes as a read or
// a write of that path would be judged by the rules, and only tightens d: a
// file a rule denies refuses the call, and one an ask rule covers makes an
// allowed call ask. The mode's own answer for the file, and its default,
// are not taken, since the call itself was judged in the mode.
func (l *Loop) filesJudged(tool tools.Tool, call model.ToolCall, d policy.Result) policy.Result {
	fa, ok := tool.(tools.FileArgs)
	if !ok || d.Decision == policy.Deny {
		return d
	}
	// Rules only: the hooks judge the call itself, with these paths in its
	// arguments, and are not told of a read or write that is not being made.
	pol := l.Policy.WithoutHooks()
	judge := func(as string, mutates bool, path string) (policy.Result, bool) {
		args, _ := json.Marshal(map[string]string{"path": path})
		r := pol.Evaluate(as, mutates, args)
		switch {
		case r.Decision == policy.Deny && r.Step != "mode":
			return policy.Result{Decision: policy.Deny, Step: "deny", Rule: r.Rule, Layer: r.Layer,
				Reason: fmt.Sprintf("the file %s it would %s: %s", path, as, r.Reason)}, true
		case r.Decision == policy.Ask && (r.Step == "ask" || r.Step == "destructive") && d.Decision == policy.Allow:
			return policy.Result{Decision: policy.Ask, Step: r.Step, Rule: r.Rule, Layer: r.Layer,
				Reason: fmt.Sprintf("the file %s it would %s: %s", path, as, r.Reason)}, true
		}
		return policy.Result{}, false
	}
	for _, p := range fa.FilesRead(call.Args) {
		if r, ok := judge("read", false, p); ok {
			if r.Decision == policy.Deny {
				return r
			}
			d = r
		}
	}
	for _, p := range fa.FilesWritten(call.Args) {
		if why := l.pathHoldsSecret(p); why != "" {
			return policy.Result{Decision: policy.Deny, Step: "deny", Reason: why}
		}
		if r, ok := judge("write", true, p); ok {
			if r.Decision == policy.Deny {
				return r
			}
			d = r
		}
	}
	return d
}
