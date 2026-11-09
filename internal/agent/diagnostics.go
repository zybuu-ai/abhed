package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// EvLSPDiagnostics is a language server's diagnostics for a file the agent
// has just written or edited, as the agent was given them; see LSPDiagnostics.
const EvLSPDiagnostics EventType = "lsp.diagnostics"

// LSPDiagnostics is the payload of lsp.diagnostics.
type LSPDiagnostics struct {
	CallID   string `json:"call_id"`
	Path     string `json:"path"`
	Language string `json:"language"`
	LSPID    string `json:"lsp_id,omitempty"`
	// Total is how many the server reported; Shown how many the agent was given.
	Total    int `json:"total"`
	Shown    int `json:"shown"`
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	// TimedOut is set when the server had not reported in time; the agent
	// was given none.
	TimedOut bool `json:"timed_out,omitempty"`
	// Skipped says why no server ran for the file's language, such as a
	// policy that did not allow its command; recorded once per language.
	Skipped    string `json:"skipped,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// Diagnosed is what a Diagnoser found: the record's payload and the text
// put after the tool's result for the agent ("" for none).
type Diagnosed struct {
	LSPDiagnostics
	Text string
}

// Diagnoser reports a language server's diagnostics for a file the agent
// has just changed. ok is false when no server is on for the file, and
// nothing is recorded.
type Diagnoser interface {
	Diagnose(ctx context.Context, l *Loop, callID, path string) (d Diagnosed, ok bool)
	Close()
}

// diagnose puts the diagnostics for the file a successful write or edit
// changed after its result, and records them.
func (l *Loop) diagnose(ctx context.Context, call model.ToolCall, result *tools.Result) {
	if l.Diagnostics == nil || result.IsError || (call.Name != "write" && call.Name != "edit") || l.Session == nil {
		return
	}
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(call.Args, &a) != nil || a.Path == "" {
		return
	}
	path, err := l.Session.Resolve(a.Path)
	if err != nil {
		return
	}
	start := time.Now()
	d, ok := l.Diagnostics.Diagnose(ctx, l, call.ID, path)
	if !ok {
		return
	}
	d.CallID = call.ID
	if d.DurationMS == 0 {
		d.DurationMS = time.Since(start).Milliseconds()
	}
	l.record(EvLSPDiagnostics, ActorSystem, d.LSPDiagnostics)
	if d.Text != "" {
		result.Content += "\n\n" + d.Text
	}
}

// BySetting is an operator's setting that answered a mode's plain ask for
// a command the engine runs, such as tools.diagnostics on.
const BySetting = "setting"

// EngineAuthorize judges and records a command the engine runs for the
// agent outside any call it made, such as the language server whose
// diagnostics follow an edit. It is recorded as a request, via names what
// issued it, then judged by policy, the secret checks and the supervisor,
// which may only make it stricter. Nobody is asked: it may run on an allow,
// or on an ask that allowed takes as answered, which the PermissionRequest
// hooks hear first and may refuse. The approval or denial is recorded too.
func (l *Loop) EngineAuthorize(ctx context.Context, call, id, via string, args json.RawMessage, allowed func(policy.Result) bool) (policy.Result, bool, error) {
	if _, err := l.Recorder.Record(EvActionRequested, ActorSystem, Trusted, ActionRequested{CallID: id, Tool: call, Args: args, Via: via}); err != nil {
		return policy.Result{Decision: policy.Deny, Reason: "the request could not be recorded", Step: "record"}, false, err
	}
	decision := l.engineDecision(ctx, call, id, args)
	if decision.Decision == policy.Ask && allowed(decision) {
		if why := l.hookRefused(ctx, model.ToolCall{ID: id, Name: call, Args: args}, decision); why != "" {
			decision = policy.Result{Decision: policy.Deny, Reason: why, Step: "hook"}
		}
	}
	ok := decision.Decision != policy.Deny && allowed(decision)
	if !ok {
		reason := decision.Reason
		if decision.Decision == policy.Ask {
			reason = "nobody is asked for it: " + reason
		}
		l.record(EvActionDenied, ActorSystem, withRule(map[string]string{
			"call_id": id, "reason": reason, "step": decision.Step, "by": ByPolicy}, decision))
		return decision, false, nil
	}
	by := ByPolicy
	if decision.Decision == policy.Ask {
		by = BySetting
	}
	approved := withRule(map[string]string{"call_id": id, "reason": decision.Reason, "step": decision.Step, "by": by}, decision)
	if _, err := l.Recorder.Record(EvActionApproved, ActorSystem, Trusted, approved); err != nil {
		l.noteRecordErr(err)
		return decision, false, err
	}
	return decision, true, nil
}

// hookRefused is the PermissionRequest hooks' refusal of an ask, or "".
func (l *Loop) hookRefused(ctx context.Context, call model.ToolCall, decision policy.Result) string {
	if l.Hooks == nil {
		return ""
	}
	return l.Hooks.PermissionRequested(ctx, l.Policy, l.sessionID(), call.Name, call.Args, decision.Reason)
}

// engineDecision is policy, the secret checks and the supervisor on a call
// the engine makes.
func (l *Loop) engineDecision(ctx context.Context, call, id string, args json.RawMessage) policy.Result {
	tool, found := l.Tools.Get(call)
	if !found {
		return policy.Result{Decision: policy.Deny, Reason: "no " + call + " tool in this session", Step: "deny"}
	}
	mutates := tools.MutatesCall(tool, args)
	decision := l.Policy.Evaluate(call, mutates, args)
	if why := l.pathSecretRefused(model.ToolCall{Name: call, Args: args}); why != "" {
		return policy.Result{Decision: policy.Deny, Reason: why, Step: "deny"}
	}
	if l.Monitor != nil && decision.Decision != policy.Deny {
		decision = l.reviewed(ctx, model.ToolCall{ID: id, Name: call, Args: args}, mutates, decision)
	}
	return decision
}
