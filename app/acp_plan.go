package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// The editor's answers to a proposed plan: the CLI's three, as permission options.
const (
	acpPlanAcceptEdits = "plan:accept-edits"
	acpPlanApprove     = "plan:approve"
	acpPlanKeep        = "plan:keep-planning"
	acpPlanConfirm     = "plan:confirm"
	acpPlanDecline     = "plan:decline"
)

// decidePlan puts a plan this prompt's run proposed to the editor as a
// permission request, and returns the message the turn goes on with, or ""
// to keep planning. Accepting moves to accept-edits or default, never auto,
// and binds the plan's runbook; steps with no undo are asked about again.
func (c *acpConn) decidePlan(ctx context.Context, s *acpSession, runErr error) string {
	if !s.inner || runErr != nil || ctx.Err() != nil {
		return ""
	}
	loop := s.parts.Loop
	plan, ok := loop.TakePlan()
	if !ok {
		return ""
	}
	callID := "plan-" + acpID()
	keep := func(note string) string {
		s.record(agent.EvPlanDecided, agent.PlanDecided{Decision: agent.PlanKeepPlanning})
		c.sessionUpdate(s.id, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": callID, "status": "failed"})
		c.sessionUpdate(s.id, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock("\n" + note + "\n")})
		return ""
	}
	text := plan.Text
	meta := map[string]any{"plan": true}
	raw := map[string]any{"plan": plan.Text}
	if rb := plan.Runbook; rb != nil {
		text += "\n\n" + runbookMarkdown(rb)
		meta["planHash"], meta["steps"] = rb.Hash, len(rb.Steps)
		raw["runbook"] = rb
	}
	c.sessionUpdate(s.id, map[string]any{"sessionUpdate": "tool_call", "toolCallId": callID, "title": "Proceed with this plan?",
		"kind": "think", "status": "pending", "rawInput": raw})

	modes := availableModes(s.parts.Config)
	var options []map[string]any
	if slices.Contains(modes, policy.ModeAcceptEdits) {
		options = append(options, map[string]any{"optionId": acpPlanAcceptEdits, "name": "Yes, and accept edits", "kind": "allow_always"})
	}
	if slices.Contains(modes, policy.ModeDefault) {
		options = append(options, map[string]any{"optionId": acpPlanApprove, "name": "Yes, ask before each change", "kind": "allow_once"})
	}
	options = append(options, map[string]any{"optionId": acpPlanKeep, "name": "No, keep planning", "kind": "reject_once"})
	answer := c.askPlan(ctx, s, callID, "Proceed with this plan?", text, raw, meta, options)
	to := map[string]policy.Mode{acpPlanAcceptEdits: policy.ModeAcceptEdits, acpPlanApprove: policy.ModeDefault}[answer]
	if to == "" || !slices.Contains(modes, to) {
		return keep("Still in plan mode; say what to change.")
	}
	if risky := destructiveSteps(plan.Runbook); len(risky) > 0 {
		spec := runbookConfirm(risky)
		confirm := c.askPlan(ctx, s, callID, spec.Title, spec.Body[0].Text, raw, map[string]any{"plan": true, "destructive": true}, []map[string]any{
			{"optionId": acpPlanConfirm, "name": "Yes, approve the steps that cannot be undone", "kind": "allow_once"},
			{"optionId": acpPlanDecline, "name": "No, keep planning", "kind": "reject_once"},
		})
		if confirm != acpPlanConfirm {
			return keep("Still in plan mode; say what to change.")
		}
	}
	if s.liveTasks() > 0 {
		return keep("Background tasks are running, so the mode was not changed and planning goes on.")
	}
	decided := agent.PlanDecided{Decision: agent.PlanAccepted, ToMode: string(to)}
	if rb := plan.Runbook; rb != nil {
		if err := loop.BindPlan(rb.Hash); err != nil {
			return keep("The runbook was not bound, so planning goes on: " + err.Error())
		}
		decided.PlanHash, decided.Runbook = rb.Hash, rb
	}
	if from := loop.Policy.Mode; from != to {
		loop.Policy.Mode = to
		s.record(agent.EvModeChanged, agent.ModeChanged{From: string(from), To: string(to), By: agent.ByUser, Via: agent.ViaPlanExit})
	}
	s.record(agent.EvPlanDecided, decided)
	loop.SetApprovedPlan(plan.Text)
	c.sessionUpdate(s.id, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": callID, "status": "completed"})
	if rb := plan.Runbook; rb != nil {
		return fmt.Sprintf("The plan is approved (mode %s). Carry it out.\n", to) + agent.RunbookPrompt(*rb)
	}
	return fmt.Sprintf("The plan is approved (mode %s). Carry it out.", to)
}

// askPlan sends one permission request about the plan and returns the
// option chosen, or "" when there was no answer.
func (c *acpConn) askPlan(ctx context.Context, s *acpSession, callID, title, text string, raw, meta map[string]any, options []map[string]any) string {
	flushed, cancel := context.WithTimeout(ctx, askFlushWait)
	_ = s.agent.Flush(flushed)
	cancel()
	res, err := c.call(ctx, "session/request_permission", map[string]any{
		"sessionId": s.id,
		"toolCall": map[string]any{"toolCallId": callID, "title": ui.VisibleLine(title), "kind": "think", "status": "pending",
			"rawInput": raw, "content": []any{map[string]any{"type": "content", "content": textBlock(text)}},
			"_meta": map[string]any{acpMetaKey: meta}},
		"options": options,
	})
	if err != nil {
		return ""
	}
	var out struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if json.Unmarshal(res, &out) != nil || out.Outcome.Outcome != "selected" {
		return ""
	}
	for _, o := range options {
		if o["optionId"] == out.Outcome.OptionID {
			return out.Outcome.OptionID
		}
	}
	return ""
}
