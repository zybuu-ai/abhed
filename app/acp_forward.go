package app

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ui"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

func init() { liveFeatures = append(liveFeatures, "suggestions") }

// forward maps the record's events onto session/update notifications as
// they are recorded.
func (c *acpConn) forward(s *acpSession, ev abhed.Event) {
	for _, u := range c.updates(s, ev, false) {
		c.sessionUpdate(s.id, u)
	}
	c.taskEvent(s, ev)
}

// replay sends a recorded conversation as the updates a live run would have,
// with the recorded tool call ids, their final statuses and each event's
// seq. Nothing is run again and nothing is asked (§4.1).
func (c *acpConn) replay(s *acpSession, events []agent.Event) {
	for _, ev := range agent.Live(events) {
		for _, u := range c.updates(s, ev, true) {
			metaOf(u)["seq"] = ev.Seq
			c.sessionUpdate(s.id, u)
		}
	}
}

// updates are the session/update payloads one event becomes. In a replay
// the whole messages stand for the deltas a live run streamed.
func (c *acpConn) updates(s *acpSession, ev abhed.Event, replay bool) []map[string]any {
	var out []map[string]any
	update := func(u map[string]any) { out = append(out, u) }
	text := textBlock
	switch ev.Type {
	case agent.EvUserMessage:
		if !replay {
			break
		}
		var p agent.Message
		if json.Unmarshal(ev.Payload, &p) == nil && p.Text != "" {
			update(map[string]any{"sessionUpdate": "user_message_chunk", "content": text(p.Text)})
		}
	case agent.EvAgentMessage:
		if !replay {
			break
		}
		var p agent.Message
		if json.Unmarshal(ev.Payload, &p) == nil && p.Text != "" {
			update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": text(p.Text)})
		}
	case agent.EvAgentDelta:
		if replay {
			break
		}
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if p.Text != "" {
			update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": text(p.Text)})
		}
	case agent.EvAgentReasoningDelta:
		if replay {
			break
		}
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if p.Text != "" {
			s.mu.Lock()
			s.thoughts = true
			s.mu.Unlock()
			update(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": text(p.Text)})
		}
	case agent.EvAgentReasoning:
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		// The whole text follows its deltas; sent once, not twice.
		s.mu.Lock()
		streamed := s.thoughts && !replay
		if !replay {
			s.thoughts = false
		}
		s.mu.Unlock()
		if p.Text != "" && !streamed {
			update(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": text(p.Text)})
		}
	case agent.EvActionRequested:
		var p agent.ActionRequested
		_ = json.Unmarshal(ev.Payload, &p)
		if p.CallID == "" {
			p.CallID = ev.ID
			s.mu.Lock()
			s.lastIdless = idlessCall{id: ev.ID, tool: p.Tool}
			s.mu.Unlock()
		}
		meta := map[string]any{"tool": p.Tool}
		if p.Via != "" {
			meta["via"] = p.Via
		}
		if ev.Actor == agent.ActorUser {
			meta["by"] = "user"
		}
		update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": p.CallID, "title": toolTitle(p.Tool, p.Args),
			"kind": toolKind(p.Tool), "status": "pending", "rawInput": p.Args, "_meta": map[string]any{acpMetaKey: meta}})
	case agent.EvActionApproved:
		var p struct {
			CallID string `json:"call_id"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if p.CallID == "" {
			// Calls are approved one at a time, right after their request.
			s.mu.Lock()
			p.CallID = s.lastIdless.id
			s.ranIdless = append(s.ranIdless, s.lastIdless)
			s.mu.Unlock()
		}
		update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": p.CallID, "status": "in_progress"})
	case agent.EvActionDenied:
		var p struct {
			CallID string `json:"call_id"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if p.CallID == "" {
			s.mu.Lock()
			p.CallID = s.lastIdless.id
			s.mu.Unlock()
		}
		update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": p.CallID, "status": "failed",
			"content": []any{map[string]any{"type": "content", "content": text("Denied: " + p.Reason)}}})
	case agent.EvObservation:
		var p agent.Observation
		_ = json.Unmarshal(ev.Payload, &p)
		if p.CallID == "" {
			p.CallID = s.takeIdless(p.Tool)
		}
		status := "completed"
		if p.IsError {
			status = "failed"
		}
		update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": p.CallID, "status": status,
			"content": []any{map[string]any{"type": "content", "content": text(p.Content)}}, "rawOutput": p.Content})
	case agent.EvSubagentAsk:
		if replay {
			break
		}
		// A subagent's call waiting on the editor: its card goes out before the ask.
		var p agent.SubagentAsk
		if json.Unmarshal(ev.Payload, &p) != nil || p.RequestID == "" {
			break
		}
		s.mu.Lock()
		if s.subAsks == nil {
			s.subAsks = map[string]bool{}
		}
		s.subAsks[p.RequestID] = true
		s.mu.Unlock()
		meta := map[string]any{"tool": p.Tool, "subagent": p.Subagent}
		if p.Via != "" {
			meta["via"] = p.Via
		}
		update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": subagentCallID(p.RequestID),
			"title": "subagent " + ui.VisibleLine(p.Subagent) + ": " + toolTitle(p.Tool, p.Args), "kind": toolKind(p.Tool),
			"status": "pending", "rawInput": p.Args, "_meta": map[string]any{acpMetaKey: meta}})
	case agent.EvSubagentAction:
		var p agent.SubagentAction
		if json.Unmarshal(ev.Payload, &p) != nil || p.RequestID == "" {
			break
		}
		s.mu.Lock()
		shown := s.subAsks[p.RequestID]
		delete(s.subAsks, p.RequestID)
		s.mu.Unlock()
		if !shown {
			break
		}
		// The call's result is in the subagent's record; the editor learns the answer.
		status, note := "completed", "Allowed; the subagent ran it."
		if p.Decision != "allowed" {
			status, note = "failed", "Denied: "+p.Reason
		}
		update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": subagentCallID(p.RequestID),
			"status": status, "content": []any{map[string]any{"type": "content", "content": text(note)}}})
	case agent.EvSubagentSpawned:
		if u := subagentCard(ev.Payload); u != nil {
			update(u)
		}
	case agent.EvSubagentReturn:
		if u := subagentReturned(ev.Payload); u != nil {
			update(u)
		}
	case agent.EvSubagentNotice:
		// Its result completes the card, whether or not a turn is open.
		var n agent.Notice
		if json.Unmarshal(ev.Payload, &n) != nil || n.TaskID == "" {
			break
		}
		status := "completed"
		if n.Status != "completed" {
			status = "failed"
		}
		update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "bg-" + n.TaskID, "status": status,
			"content": []any{map[string]any{"type": "content", "content": text(n.Content)}}})
	case agent.EvSessionWoken:
		// A turn the session started for finished background work, marked as such.
		var w agent.SessionWoken
		if json.Unmarshal(ev.Payload, &w) != nil {
			break
		}
		update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": text(wokenLine(s, w) + "\n\n"),
			"_meta": map[string]any{acpMetaKey: map[string]any{"woken": map[string]any{"by": w.By, "taskIds": w.TaskIDs}}}})
	case agent.EvTodoUpdated:
		var p agent.TodoList
		_ = json.Unmarshal(ev.Payload, &p)
		entries := make([]map[string]any, 0, len(p.Items))
		for _, it := range p.Items {
			st := "pending"
			switch it.Status {
			case "in_progress":
				st = "in_progress"
			case "done":
				st = "completed"
			}
			entries = append(entries, map[string]any{"content": it.Text, "priority": "medium", "status": st})
		}
		update(map[string]any{"sessionUpdate": "plan", "entries": entries})
	case agent.EvSuggestionOffered:
		// A next prompt for the editor's input, before the reply that ends the turn.
		var p agent.SuggestionOffered
		if replay || json.Unmarshal(ev.Payload, &p) != nil || p.Text == "" {
			break
		}
		update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": text(""),
			"_meta": map[string]any{acpMetaKey: map[string]any{"suggestion": agent.CleanSuggestion(p.Text)}}})
	case agent.EvModelCall:
		if replay {
			break
		}
		var p agent.ModelCall
		_ = json.Unmarshal(ev.Payload, &p)
		s.mu.Lock()
		s.totalIn += p.TokensIn
		s.totalOut += p.TokensOut
		totalIn, totalOut := s.totalIn, s.totalOut
		s.mu.Unlock()
		// A suggestion's call is counted, but says nothing of the context.
		if p.ContextWindow > 0 && p.Purpose == "" {
			// No cost: the engine has no price table, and invents none.
			update(map[string]any{"sessionUpdate": "usage_update", "used": p.TokensIn, "size": p.ContextWindow,
				"_meta": map[string]any{acpMetaKey: map[string]any{"tokensIn": p.TokensIn, "tokensOut": p.TokensOut,
					"tokensCached": p.TokensCached, "sessionTotalIn": totalIn, "sessionTotalOut": totalOut}}})
		}
	case agent.EvModelFallback:
		// The engine changed an option itself: every option goes out again.
		if replay {
			break
		}
		if opts := c.configOptions(s); len(opts) > 0 {
			update(map[string]any{"sessionUpdate": "config_option_update", "configOptions": opts})
		}
	case agent.EvModeChanged:
		var p agent.ModeChanged
		if json.Unmarshal(ev.Payload, &p) == nil && p.To != "" {
			update(map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": p.To})
		}
	}
	return out
}

// subagentCard opens a card for a subagent as it starts: "bg-<task>" for a
// background task, "sub-<session>" for one the turn waits on (§6.1, §6.2).
func subagentCard(payload json.RawMessage) map[string]any {
	var p struct {
		Session     string `json:"session"`
		Description string `json:"description"`
		AgentType   string `json:"agent_type"`
		Depth       int    `json:"depth"`
		Model       string `json:"model"`
		Provider    string `json:"provider"`
		Branch      string `json:"branch"`
		SHA256      string `json:"definition_sha256"`
		Background  bool   `json:"background"`
		TaskID      string `json:"task_id"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return nil
	}
	sub := map[string]any{"session": p.Session, "agentType": p.AgentType, "depth": p.Depth, "model": p.Model}
	for k, v := range map[string]string{"provider": p.Provider, "branch": p.Branch, "definitionSha256": p.SHA256} {
		if v != "" {
			sub[k] = v
		}
	}
	if p.Background && p.TaskID != "" {
		return map[string]any{"sessionUpdate": "tool_call", "toolCallId": "bg-" + p.TaskID,
			"title": "background: " + ui.VisibleLine(p.Description), "kind": "think", "status": "in_progress",
			"_meta": map[string]any{acpMetaKey: map[string]any{"taskId": p.TaskID, "subagent": sub}}}
	}
	if p.Session == "" {
		return nil
	}
	kind := p.AgentType
	if kind == "" {
		kind = "general"
	}
	return map[string]any{"sessionUpdate": "tool_call", "toolCallId": "sub-" + p.Session,
		"title": ui.VisibleLine("subagent " + kind + ": " + p.Description), "kind": "think", "status": "in_progress",
		"_meta": map[string]any{acpMetaKey: map[string]any{"subagent": sub}}}
}

// subagentReturned closes a foreground subagent's card with what it cost; a
// background task's card is closed by its notice instead.
func subagentReturned(payload json.RawMessage) map[string]any {
	var p struct {
		Session      string `json:"session"`
		Reason       string `json:"reason"`
		Turns        int    `json:"turns"`
		TokensIn     int    `json:"tokens_in"`
		TokensOut    int    `json:"tokens_out"`
		SummaryChars int    `json:"summary_chars"`
		Background   bool   `json:"background"`
	}
	if json.Unmarshal(payload, &p) != nil || p.Session == "" || p.Background {
		return nil
	}
	status := "completed"
	if p.Reason != string(agent.TermCompleted) {
		status = "failed"
	}
	return map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "sub-" + p.Session, "status": status,
		"content": []any{map[string]any{"type": "content", "content": textBlock(fmt.Sprintf(
			"Returned (%s): %d turns, %d tokens in, %d out; a %d-character summary.",
			ui.VisibleLine(p.Reason), p.Turns, p.TokensIn, p.TokensOut, p.SummaryChars))}},
		"_meta": map[string]any{acpMetaKey: map[string]any{"turns": p.Turns, "tokensIn": p.TokensIn,
			"tokensOut": p.TokensOut, "summaryChars": p.SummaryChars, "reason": p.Reason}}}
}

// wokenLine names the tasks a woken turn continues from.
func wokenLine(s *acpSession, w agent.SessionWoken) string {
	if w.By == "caller" {
		return "Continuing with background results, as asked."
	}
	names := make([]string, 0, len(w.TaskIDs))
	for _, id := range w.TaskIDs {
		name := id
		if ti, ok := s.task(id); ok && ti.Description != "" {
			name = ti.Description
		}
		names = append(names, ui.VisibleLine(name))
	}
	if len(names) == 0 {
		return "Continuing with background results."
	}
	return "Continuing with results from " + strings.Join(names, ", ") + "."
}
