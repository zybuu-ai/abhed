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
	live, ok := s.session(r.Context(), id, TenantOf(r.Context()), UserOf(r.Context()))
	if !ok {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
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
	if req.PlanHash == "" || req.PlanHash != plan.Hash {
		WriteError(w, http.StatusConflict, "the answer is for another plan than the one waiting")
		return
	}
	record := func(t agent.EventType, payload any) error {
		_, err := loop.Recorder.Record(t, agent.ActorUser, agent.Trusted, payload)
		return err
	}
	configured := policy.Mode(orDefaultStr(s.opts.Config.Permissions.Mode, string(policy.ModeDefault)))
	to := map[string]policy.Mode{planAcceptEdits: policy.ModeAcceptEdits, planApprove: policy.ModeDefault}[req.Decision]
	switch {
	case req.Decision == planKeep:
		loop.TakePlan()
		if err := record(agent.EvPlanDecided, agent.PlanDecided{Decision: agent.PlanKeepPlanning}); err != nil {
			WriteError(w, http.StatusInternalServerError, "the answer could not be recorded")
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
	if n := undoable(plan.Runbook); n > 0 && !req.ConfirmUndo {
		WriteError(w, http.StatusPreconditionRequired, fmt.Sprintf("%d steps of the runbook cannot be undone; confirm them with confirm_no_undo", n))
		return
	}
	loop.TakePlan()
	decided := agent.PlanDecided{Decision: agent.PlanAccepted, ToMode: string(to)}
	if rb := plan.Runbook; rb != nil {
		if err := loop.BindPlan(rb.Hash); err != nil {
			WriteError(w, http.StatusConflict, "the runbook was not bound: "+err.Error())
			return
		}
		decided.PlanHash, decided.Runbook = rb.Hash, rb
	}
	if from := loop.Policy.Mode; from != to {
		if err := record(agent.EvModeChanged, agent.ModeChanged{From: string(from), To: string(to), By: agent.ByUser, Via: agent.ViaPlanExit}); err != nil {
			loop.UnbindPlan()
			WriteError(w, http.StatusInternalServerError, "the mode change could not be recorded, so it was not made")
			return
		}
		loop.Policy.Mode = to
	}
	if err := record(agent.EvPlanDecided, decided); err != nil {
		s.log.Error("plan decision not recorded", "session", id, "error", err)
	}
	loop.SetApprovedPlan(plan.Text)
	msg := fmt.Sprintf("The plan is approved (mode %s). Carry it out.", to)
	if rb := plan.Runbook; rb != nil {
		msg += "\n" + agent.RunbookPrompt(*rb)
	}
	WriteJSON(w, http.StatusOK, map[string]string{"decision": req.Decision, "mode": string(to), "message": msg})
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
