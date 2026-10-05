package extension

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// Host runs the configured extensions and combines their answers.
//
// Combination is always toward the stricter outcome. Where two extensions
// disagree about a tool call, the blocking one wins; where one asks for
// approval and another says nothing, the call is asked. This is what makes the
// order they run in irrelevant to the decision, which in turn is what makes the
// audit trail reproducible: the same call and the same extension set produce the
// same verdict regardless of scheduling.
type Host struct {
	exts []*Extension
	logf func(string, ...any)

	firedMu sync.RWMutex
	onFired func(Fired)
}

// Verdicts a hook can reach, as hook.fired records them. There is no allow.
const (
	VerdictBlock    = "block"
	VerdictAsk      = "ask"
	VerdictAnnotate = "annotate"
)

// Fired is one hook's verdict on one event: it blocked, forced an ask, or
// only said something. A hook that said nothing did not fire.
type Fired struct {
	Extension string
	Event     Event
	Verdict   string
	Reason    string
}

// SetOnFired sets what is told of each hook that fired, for the record.
func (h *Host) SetOnFired(f func(Fired)) {
	h.firedMu.Lock()
	h.onFired = f
	h.firedMu.Unlock()
}

func (h *Host) fired(f Fired) {
	h.firedMu.RLock()
	on := h.onFired
	h.firedMu.RUnlock()
	if on != nil {
		on(f)
	}
}

// Extensions are the started extensions, in load order.
func (h *Host) Extensions() []*Extension {
	if h == nil {
		return nil
	}
	return append([]*Extension(nil), h.exts...)
}

func NewHost(logf func(string, ...any)) *Host {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Host{logf: logf}
}

// Load starts each configured extension. One that fails to start is reported
// and skipped: a broken plugin must not stop the agent from running.
func (h *Host) Load(ctx context.Context, cfgs []Config) []error {
	var errs []error
	for _, cfg := range cfgs {
		e, err := Start(ctx, cfg, h.logf)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		h.exts = append(h.exts, e)
	}
	return errs
}

func (h *Host) Len() int { return len(h.exts) }

// Names lists the loaded extensions, for `abhed doctor`.
func (h *Host) Names() []string {
	out := make([]string, 0, len(h.exts))
	for _, e := range h.exts {
		out = append(out, e.Name())
	}
	return out
}

// Running maps each started extension's name to whether it is still asked.
func (h *Host) Running() map[string]bool {
	out := make(map[string]bool, len(h.exts))
	for _, e := range h.exts {
		out[e.Name()] = e.Running()
	}
	return out
}

func (h *Host) Close() {
	for _, e := range h.exts {
		_ = e.Close()
	}
}

// ToolCallDecision is the combined verdict on one tool call.
type ToolCallDecision struct {
	Block  bool
	Ask    bool
	Reason string
	// Args, when non-nil, replaces the call's arguments.
	Args json.RawMessage
}

// OnToolCall asks every subscribed extension whose matcher takes the call.
//
// The result can only ever be the same or stricter than what policy decided.
// An extension that returns nothing leaves the decision untouched. One that
// crashes or times out on the call fails closed: that call is blocked, and
// every later one is asked, since the veto it stood for is gone.
func (h *Host) OnToolCall(ctx context.Context, sessionID, tool string, args json.RawMessage) ToolCallDecision {
	return h.onToolCall(ctx, sessionID, tool, args, nil)
}

func (h *Host) onToolCall(ctx context.Context, sessionID, tool string, args json.RawMessage, p *policy.Engine) ToolCallDecision {
	out := ToolCallDecision{}
	for _, e := range h.exts {
		if !e.wants(EvToolCall) || !e.Matches(p, tool, args) {
			continue
		}
		reply, err := e.call(ctx, Request{
			Event: EvToolCall, SessionID: sessionID, Tool: tool, Args: args,
		})
		switch {
		case errors.Is(err, errNotRunning):
			reply = Reply{Ask: true, Reason: "extension " + e.Name() + " is not running, so each call it would have screened is asked"}
		case err != nil:
			reply = Reply{Block: true, Reason: "extension " + e.Name() + " did not answer (" + err.Error() + "), so the call is refused"}
		}
		if reply.Block {
			// The first block is final: nothing another extension says can
			// unblock it, so there is no reason to keep asking.
			out.Block = true
			out.Reason = firstNonEmpty(reply.Reason, "blocked by extension "+e.Name())
			h.fired(Fired{Extension: e.Name(), Event: EvToolCall, Verdict: VerdictBlock, Reason: out.Reason})
			return out
		}
		if reply.Ask {
			out.Ask = true
			why := firstNonEmpty(reply.Reason, "extension "+e.Name()+" requires approval")
			if out.Reason == "" {
				out.Reason = why
			}
			h.fired(Fired{Extension: e.Name(), Event: EvToolCall, Verdict: VerdictAsk, Reason: why})
		} else if reply.Reason != "" || reply.Log != "" {
			// A hook that only explains itself is on the record, as for every other event.
			h.fired(Fired{Extension: e.Name(), Event: EvToolCall, Verdict: VerdictAnnotate, Reason: firstNonEmpty(reply.Reason, reply.Log)})
		}
		if len(reply.Args) > 0 {
			// A later extension sees the rewritten arguments, so a chain
			// composes rather than the last writer winning.
			args = reply.Args
			out.Args = reply.Args
		}
	}
	return out
}

// Veto asks the extensions that take ev, a user_prompt_submit or
// permission_request, whether to refuse what it is about, and returns why
// when one does. Nothing a reply says approves anything. A permission_request
// hook that crashes or hangs refuses the call, as a tool_call hook does.
func (h *Host) Veto(ctx context.Context, ev Event, req Request) string {
	req.Event = ev
	for _, e := range h.exts {
		if !e.wants(ev) || req.Tool != "" && !e.Matches(req.Policy, req.Tool, req.Args) {
			continue
		}
		reply, err := e.call(ctx, req)
		if err != nil && ev == EvPermissionRequest && !errors.Is(err, errNotRunning) {
			reply = Reply{Block: true, Reason: "extension " + e.Name() + " did not answer (" + err.Error() + "), so the call is refused"}
		}
		switch {
		case reply.Block:
			why := firstNonEmpty(reply.Reason, "blocked by extension "+e.Name())
			h.fired(Fired{Extension: e.Name(), Event: ev, Verdict: VerdictBlock, Reason: why})
			return why
		case reply.Ask || reply.Reason != "" || reply.Log != "":
			h.fired(Fired{Extension: e.Name(), Event: ev, Verdict: VerdictAnnotate, Reason: firstNonEmpty(reply.Reason, reply.Log)})
		}
	}
	return ""
}

// NotRunning names the extensions that take ev but have stopped, so a
// caller can say what went unscreened.
func (h *Host) NotRunning(ev Event) []string {
	var out []string
	for _, e := range h.exts {
		if e.wants(ev) && !e.Running() {
			out = append(out, e.Name())
		}
	}
	return out
}

// Observe tells the extensions that take ev, one that only observes, what
// happened. An async extension is not waited for; one that answers with a
// reason or a log line is recorded as annotating.
func (h *Host) Observe(ctx context.Context, ev Event, req Request) {
	req.Event = ev
	for _, e := range h.exts {
		if !e.Subscribed(ev) {
			continue
		}
		if e.cfg.Async {
			go e.Call(context.WithoutCancel(ctx), req)
			continue
		}
		if reply := e.Call(ctx, req); reply.Reason != "" || reply.Log != "" {
			h.fired(Fired{Extension: e.Name(), Event: ev, Verdict: VerdictAnnotate, Reason: firstNonEmpty(reply.Reason, reply.Log)})
		}
	}
}

// OnToolResult lets extensions rewrite a result before the model reads it.
func (h *Host) OnToolResult(ctx context.Context, sessionID, tool, content string, isErr bool) (string, bool) {
	for _, e := range h.exts {
		if !e.Subscribed(EvToolResult) {
			continue
		}
		reply := e.Call(ctx, Request{
			Event: EvToolResult, SessionID: sessionID, Tool: tool,
			Content: content, IsError: isErr,
		})
		if reply.Content != nil {
			content = *reply.Content
		}
		if reply.IsError != nil {
			isErr = *reply.IsError
		}
	}
	return content, isErr
}

// OnContext lets extensions drop messages before a model call.
//
// Only removal is possible: an extension chooses which of the messages Abhed
// built may be sent, and cannot add or alter one. Redaction and context
// trimming are expressible; smuggling an instruction into history is not.
func (h *Host) OnContext(ctx context.Context, sessionID string, msgs []Message) []int {
	keep := make([]int, len(msgs))
	for i := range msgs {
		keep[i] = i
	}
	for _, e := range h.exts {
		if !e.Subscribed(EvContext) {
			continue
		}
		reply := e.Call(ctx, Request{
			Event: EvContext, SessionID: sessionID, Messages: subset(msgs, keep),
		})
		if reply.Keep == nil {
			continue // no opinion
		}
		// Indices are into what this extension was shown, so they are mapped
		// back through the current selection. Anything out of range is
		// ignored rather than trusted.
		next := make([]int, 0, len(reply.Keep))
		for _, i := range reply.Keep {
			if i >= 0 && i < len(keep) {
				next = append(next, keep[i])
			}
		}
		keep = next
	}
	return keep
}

// OnBeforeAgentStart collects system-prompt additions.
func (h *Host) OnBeforeAgentStart(ctx context.Context, sessionID, system string) string {
	var add []string
	for _, e := range h.exts {
		if !e.Subscribed(EvBeforeAgentStart) {
			continue
		}
		reply := e.Call(ctx, Request{
			Event: EvBeforeAgentStart, SessionID: sessionID, System: system,
		})
		if s := reply.System; s != "" {
			add = append(add, s)
		}
	}
	if len(add) == 0 {
		return system
	}
	out := system
	for _, a := range add {
		out += "\n\n" + a
	}
	return out
}

// Notify sends a fire-and-forget lifecycle event.
func (h *Host) Notify(ctx context.Context, ev Event, sessionID string) {
	for _, e := range h.exts {
		if e.Subscribed(ev) {
			e.Call(ctx, Request{Event: ev, SessionID: sessionID})
		}
	}
}

// PolicyHook adapts the host to the policy engine.
//
// It returns a Result only to make a decision stricter — Deny or Ask — and
// never Allow. The engine evaluates hooks first so they can veto, which means
// a hook that returned Allow would short-circuit the deny rules beneath it.
// That is acceptable for a hook an operator compiled in; it is not acceptable
// for one an operator dropped into a directory. Refusing to construct Allow
// here is what keeps deny absolute.
func (h *Host) PolicyHook(ctx context.Context, sessionID string) policy.Hook {
	return h.PolicyHookFor(ctx, sessionID, nil)
}

// PolicyHookFor is PolicyHook for engine p, whose roots place the relative
// paths in extensions' match rules.
func (h *Host) PolicyHookFor(ctx context.Context, sessionID string, p *policy.Engine) policy.Hook {
	return func(tool string, args json.RawMessage) *policy.Result {
		d := h.onToolCall(ctx, sessionID, tool, args, p)
		switch {
		case d.Block:
			return &policy.Result{Decision: policy.Deny, Reason: d.Reason}
		case d.Ask:
			return &policy.Result{Decision: policy.Ask, Reason: d.Reason}
		}
		return nil // no opinion: policy decides
	}
}

func subset(msgs []Message, keep []int) []Message {
	out := make([]Message, 0, len(keep))
	for _, i := range keep {
		if i >= 0 && i < len(msgs) {
			out = append(out, msgs[i])
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// OnBeforeCompact asks extensions about a pending compaction.
//
// An extension may cancel it, or supply the summary itself. Summarizing is the
// one place where the harness discards information on purpose, and the default
// summarizer cannot know that this deployment must keep the ticket number, the
// customer id, or whatever else the next turn will be judged against.
//
// The first extension to answer wins, and a cancel outranks a summary: both are
// the stricter reading of "do not summarize this the usual way".
func (h *Host) OnBeforeCompact(ctx context.Context, sessionID string, msgs []Message) (summary string, cancel bool) {
	for _, e := range h.exts {
		if !e.Subscribed(EvBeforeCompact) {
			continue
		}
		reply := e.Call(ctx, Request{
			Event: EvBeforeCompact, SessionID: sessionID, Messages: msgs,
		})
		if reply.Cancel {
			return "", true
		}
		if reply.Summary != "" && summary == "" {
			summary = reply.Summary
		}
	}
	return summary, false
}
