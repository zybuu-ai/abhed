package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/zybuu-ai/abhed/internal/agent"
)

type forkRequest struct {
	// BeforeSeq is the step of a message of the person's: the conversation
	// goes on from just before it.
	BeforeSeq int64 `json:"before_seq"`
}

type forkResponse struct {
	Kept int `json:"kept"`
}

// forkSession takes the conversation back to just before one of the person's
// messages, so it can be sent again as written or edited. It is the engine's
// fork: recorded as conversation.forked, with the abandoned steps kept in the
// record. Files are not touched. Between turns only, with nothing queued, and
// refused while background tasks run, as the command line's /fork is.
func (s *Server) forkSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validSessionID(id) {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	capBody(w, r)
	var req forkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badBody(w, err, "before_seq names the step of a message of yours")
		return
	}
	if req.BeforeSeq <= 0 {
		WriteError(w, http.StatusBadRequest, "before_seq names the step of a message of yours")
		return
	}
	if s.draining.Load() {
		w.Header().Set("Retry-After", "5")
		WriteError(w, http.StatusServiceUnavailable, errDraining.Error())
		return
	}
	live, status, msg := s.liveOrReopened(r, id)
	if live == nil {
		if status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "5")
		}
		WriteError(w, status, msg)
		return
	}

	// Held as a message holds it, so no turn starts between the checks and the fork.
	live.claimMu.Lock()
	defer live.claimMu.Unlock()
	if s.draining.Load() {
		w.Header().Set("Retry-After", "5")
		WriteError(w, http.StatusServiceUnavailable, errDraining.Error())
		return
	}
	newly, err := s.claimLocked(r.Context(), id, live)
	switch {
	case errors.Is(err, errBusySession):
		WriteError(w, http.StatusConflict, "session is being continued elsewhere")
		return
	case errors.Is(err, errHoldFailed):
		writeHoldFailed(w)
		return
	case err != nil:
		s.log.Error("claim failed", "session", id, "error", err)
		WriteError(w, http.StatusInternalServerError, "could not fork the session")
		return
	}
	if newly {
		s.holdClaim(id, live)
	}
	live.mu.Lock()
	busy := live.State == "running" || live.State == "waiting_approval"
	live.mu.Unlock()
	if busy {
		WriteError(w, http.StatusConflict, "the session is mid-turn; stop it or wait for the turn to finish")
		return
	}
	// A queued message would be read after the fork, in a conversation it was not written for.
	if len(live.Loop.Queued()) > 0 {
		WriteError(w, http.StatusConflict, "messages are still queued; send or cancel them first")
		return
	}
	// Read after the claim: another process may have extended the record before it.
	events, err := s.store.Events(id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "the record could not be read")
		return
	}
	mine := false
	for _, ev := range agent.Live(events) {
		if ev.Seq == req.BeforeSeq {
			mine = ev.Type == agent.EvUserMessage
			break
		}
	}
	if !mine {
		WriteError(w, http.StatusBadRequest, "that step is not a message of yours in the conversation as it stands")
		return
	}
	kept, err := live.Loop.ForkBefore(events, req.BeforeSeq)
	if err != nil {
		if errors.Is(err, errBusySession) || errors.Is(err, errHoldFailed) {
			writeUnrecorded(w, err, "the session was not forked")
			return
		}
		// Running background tasks, named, or a record that refused the marker: nothing was changed.
		WriteError(w, http.StatusConflict, "not forked: "+err.Error())
		return
	}
	s.log.Info("session forked", "session", id, "before", req.BeforeSeq, "user", UserOf(r.Context()))
	WriteJSON(w, http.StatusOK, forkResponse{Kept: kept})
}
