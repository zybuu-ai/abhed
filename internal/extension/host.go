package extension

import (
	"context"
	"encoding/json"

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

// OnToolCall asks every subscribed extension about a call.
//
// The result can only ever be the same or stricter than what policy decided.
// An extension that returns nothing, crashes, or times out leaves the decision
// untouched — which is why a misbehaving extension cannot widen access.
func (h *Host) OnToolCall(ctx context.Context, sessionID, tool string, args json.RawMessage) ToolCallDecision {
	out := ToolCallDecision{}
	for _, e := range h.exts {
		if !e.Subscribed(EvToolCall) {
			continue
		}
		reply := e.Call(ctx, Request{
			Event: EvToolCall, SessionID: sessionID, Tool: tool, Args: args,
		})
		if reply.Block {
			// The first block is final: nothing another extension says can
			// unblock it, so there is no reason to keep asking.
			out.Block = true
			out.Reason = firstNonEmpty(reply.Reason, "blocked by extension "+e.Name())
			return out
		}
		if reply.Ask {
			out.Ask = true
			if out.Reason == "" {
				out.Reason = firstNonEmpty(reply.Reason, "extension "+e.Name()+" requires approval")
			}
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
	return func(tool string, args json.RawMessage) *policy.Result {
		d := h.OnToolCall(ctx, sessionID, tool, args)
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
