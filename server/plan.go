package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// The workbench's answers to a proposed plan, as the CLI offers them.
const (
	planAcceptEdits = "accept-edits"
	planApprove     = "approve"
	planKeep        = "keep-planning"
)

// decidePlan is the person's answer to the plan the agent proposed in plan
// mode. Accepting moves the session to default, or to accept-edits where
// that is the configured mode, never further; it binds the plan's runbook,
// and a runbook with steps that cannot be undone needs confirm_no_undo. The
// reply carries the message the conversation goes on with, sent as any other.
func (s *Server) decidePlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Decision    string `json:"decision"`
		PlanHash    string `json:"plan_hash"`
		ConfirmUndo bool   `json:"confirm_no_undo"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badBody(w, err, "invalid JSON body")
		return
	}
	user := UserOf(r.Context())
	live, ok := s.session(r.Context(), id, TenantOf(r.Context()), user)
	owner := ok
	if !ok {
		// An approver the edition's gate lets answer this session's asks may
		// answer its runbook too: that is one approval of every step.
		live, ok = s.approverSession(r.Context(), id, TenantOf(r.Context()), user)
	}
	if !ok {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	s.answerPlan(w, r, live, owner, req.Decision, req.PlanHash, req.ConfirmUndo, false)
}

// planRequest names a waiting plan's runbook as a request: in PendingAsks,
// to the approval gate, and as the request_id an answer on
// POST /v1/sessions/{id}/approve gives.
const planRequest = "plan:"

// answerPlan answers the plan waiting in live, from POST /plan or, through
// the approval queue, POST /approve with request_id plan:<hash>; queued
// says the latter, whose reply carries no message to send. A plan an
// approver accepts goes on with its message queued for the owner's next run.
func (s *Server) answerPlan(w http.ResponseWriter, r *http.Request, live *liveSession, owner bool, decision, hash string, confirmUndo, queued bool) {
	user, id := UserOf(r.Context()), live.ID
	// Held as a message holds it, so no turn starts while the mode changes.
	live.claimMu.Lock()
	defer live.claimMu.Unlock()
	live.mu.Lock()
	busy := live.ran != nil
	live.mu.Unlock()
	if busy {
		WriteError(w, http.StatusConflict, "the session is mid-turn; answer the plan when the turn ends")
		return
	}
	loop := live.Loop
	plan, ok := loop.PendingPlan()
	if !ok {
		WriteError(w, http.StatusConflict, "no plan is waiting for an answer in this session")
		return
	}
	if hash == "" || hash != plan.Hash {
		WriteError(w, http.StatusConflict, "the answer is for another plan than the one waiting")
		return
	}
	if !owner && plan.Runbook == nil {
		WriteError(w, http.StatusForbidden, "only the session's owner answers a plan without a runbook")
		return
	}
	record := func(t agent.EventType, payload any) error {
		_, err := loop.Recorder.Record(t, agent.ActorUser, agent.Trusted, payload)
		return err
	}
	configured := policy.Mode(orDefaultStr(s.opts.Config.Permissions.Mode, string(policy.ModeDefault)))
	to := map[string]policy.Mode{planAcceptEdits: policy.ModeAcceptEdits, planApprove: policy.ModeDefault}[decision]
	switch {
	case decision == planKeep:
		// The owner withdraws their own plan; an approver's refusal is an answer the gate weighs.
		if !owner && !s.gateAnswer(w, r, s.planAnswer(live, plan, user, false)) {
			return
		}
		if err := record(agent.EvPlanDecided, agent.PlanDecided{Decision: agent.PlanKeepPlanning, By: user}); err != nil {
			WriteError(w, http.StatusInternalServerError, "the answer could not be recorded")
			return
		}
		loop.TakePlan()
		if queued {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]string{"decision": planKeep})
		return
	case to == "":
		WriteError(w, http.StatusBadRequest, "decision is accept-edits, approve or keep-planning")
		return
	case to == policy.ModeAcceptEdits && configured != policy.ModeAcceptEdits:
		WriteError(w, http.StatusForbidden, "accept-edits is not this server's configured mode; approve to be asked before each change")
		return
	case loop.Background.Live() > 0:
		WriteError(w, http.StatusConflict, "background tasks are running in this session; answer the plan once they end")
		return
	}
	if n := undoable(plan.Runbook); n > 0 && !confirmUndo {
		WriteError(w, http.StatusPreconditionRequired, fmt.Sprintf("%d steps of the runbook cannot be undone; confirm them with confirm_no_undo", n))
		return
	}
	if err := loop.PlanOffered(plan); err != nil {
		WriteError(w, http.StatusConflict, "the runbook cannot be bound: "+err.Error())
		return
	}
	// A runbook answers asks, so the gate that decides who answers them, and
	// when an answer settles, decides this one: refused, held or settled.
	if plan.Runbook != nil && !s.gateAnswer(w, r, s.planAnswer(live, plan, user, true)) {
		return
	}
	// Each change is on record before it takes effect; the plan stays
	// waiting until the runbook is bound.
	if from := loop.Policy.Mode; from != to {
		if err := record(agent.EvModeChanged, agent.ModeChanged{From: string(from), To: string(to), By: agent.ByUser, Via: agent.ViaPlanExit}); err != nil {
			WriteError(w, http.StatusInternalServerError, "the mode change could not be recorded, so it was not made")
			return
		}
		loop.Policy.Mode = to
	}
	decided := agent.PlanDecided{Decision: agent.PlanAccepted, ToMode: string(to), By: user}
	if rb := plan.Runbook; rb != nil {
		decided.PlanHash, decided.Runbook = rb.Hash, rb
	}
	if err := record(agent.EvPlanDecided, decided); err != nil {
		loop.UnbindPlan()
		s.log.Error("plan decision not recorded", "session", id, "error", err)
		WriteError(w, http.StatusInternalServerError, "the answer could not be recorded, so no runbook is bound")
		return
	}
	if err := loop.AcceptPlan(plan); err != nil {
		WriteError(w, http.StatusConflict, "the runbook was not bound: "+err.Error())
		return
	}
	loop.TakePlan()
	loop.SetApprovedPlan(plan.Text)
	msg := fmt.Sprintf("The plan is approved (mode %s). Carry it out.", to)
	if rb := plan.Runbook; rb != nil {
		msg += "\n" + agent.RunbookPrompt(*rb)
	}
	if !owner || queued {
		// The owner's next run reads it first; nobody else sends to their session.
		loop.Queue(msg)
	}
	if queued {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"decision": decision, "mode": string(to), "message": msg})
}

// pendingPlanAsk is the plan waiting in l with a runbook, as the approval
// queue lists it, or false. Called without l.mu held.
func pendingPlanAsk(l *liveSession, tenant, owner string) (PendingAsk, bool) {
	if l.Loop == nil {
		return PendingAsk{}, false
	}
	plan, ok := l.Loop.PendingPlan()
	if !ok || plan.Runbook == nil {
		return PendingAsk{}, false
	}
	return PendingAsk{SessionID: l.ID, Tenant: tenant, Owner: owner, Request: planRequest + plan.Hash,
		RequestID: planRequest + plan.Hash, Tool: "exit_plan", Args: redactedArgs(l, planSummary(plan)),
		Reason: planReason(plan), Since: l.Loop.PendingPlanSince()}, true
}

// planSummary is a runbook as an approver reads it in a queue: the plan's
// text, clipped, and each step on one line.
func planSummary(plan agent.PlanProposed) json.RawMessage {
	text := []rune(plan.Text)
	if len(text) > 1000 {
		text = append(text[:1000], '…')
	}
	var steps []string
	for _, st := range plan.Runbook.Steps {
		line := fmt.Sprintf("%d. %s %s %s", st.Index, st.Tool, st.Target, stepWhat(st))
		if st.Destructive != "" {
			line += " (no undo: " + st.Destructive + ")"
		}
		steps = append(steps, line)
	}
	out, _ := json.Marshal(map[string]any{"plan": string(text), "steps": steps})
	return out
}

// stepWhat is what a step runs, on one line.
func stepWhat(st agent.RunbookStep) string {
	var a map[string]any
	if json.Unmarshal(st.Args, &a) == nil {
		if c, ok := a["command"].(string); ok {
			return c
		}
	}
	return string(st.Args)
}

// planReason says what approving a runbook approves.
func planReason(plan agent.PlanProposed) string {
	why := fmt.Sprintf("a runbook of %d steps, plan %s: approving it runs each step made exactly as listed without asking", len(plan.Runbook.Steps), plan.Hash)
	if n := undoable(plan.Runbook); n > 0 {
		why += fmt.Sprintf("; %d cannot be undone", n)
	}
	return why
}

// planAnswer is an answer to a plan's runbook as the approval gate is told
// it: one request, named by the plan's hash, for every step.
func (s *Server) planAnswer(live *liveSession, plan agent.PlanProposed, by string, approved bool) ApprovalAnswer {
	s.mu.RLock()
	tenant, owner := live.Tenant, live.User
	s.mu.RUnlock()
	return ApprovalAnswer{Tenant: tenant, SessionID: live.ID, Owner: owner, Request: planRequest + plan.Hash,
		Tool: "exit_plan", Args: redactedArgs(live, planSummary(plan)), Reason: planReason(plan),
		Since: live.Loop.PendingPlanSince(), By: by, Approved: approved}
}

// undoable counts a runbook's steps that cannot be undone.
func undoable(rb *agent.Runbook) int {
	n := 0
	if rb != nil {
		for _, st := range rb.Steps {
			if st.Destructive != "" {
				n++
			}
		}
	}
	return n
}
