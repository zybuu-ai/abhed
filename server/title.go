package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// maxTitleRunes bounds a session title: a label for the list, not a note.
const maxTitleRunes = 120

type titleRequest struct {
	Title string `json:"title"`
}

type titleResponse struct {
	Title string `json:"title"`
	From  string `json:"from,omitempty"`
}

// renameSession gives a session a title, for its owner. The rename is recorded
// as session.renamed before the list shows it, so the record says who named
// a session what; the opening prompt is never changed. An empty title clears it.
func (s *Server) renameSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validSessionID(id) {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	capBody(w, r)
	var req titleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badBody(w, err, "invalid request")
		return
	}
	title, msg := cleanTitle(req.Title)
	if msg != "" {
		WriteError(w, http.StatusBadRequest, msg)
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

	// Held as a model switch holds it, so no turn starts between the check and the record.
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
		WriteError(w, http.StatusInternalServerError, "could not rename the session")
		return
	}
	if newly {
		s.holdClaim(id, live)
	}
	live.mu.Lock()
	busy := live.State == "running" || live.State == "waiting_approval"
	from := live.title
	live.mu.Unlock()
	// Between turns only, as a model switch is: a rename never lands inside a run's steps.
	if busy {
		WriteError(w, http.StatusConflict, "the session is mid-turn; rename it once the turn has finished")
		return
	}
	if _, err := live.Loop.Recorder.Record(agent.EvSessionRenamed, agent.ActorUser, agent.Trusted,
		agent.SessionRenamed{Title: title, From: from, By: UserOf(r.Context())}); err != nil {
		s.log.Error("session rename not recorded", "session", id, "error", err)
		writeUnrecorded(w, err, "the session was not renamed: the rename could not be recorded")
		return
	}
	live.mu.Lock()
	live.title = title
	live.mu.Unlock()
	WriteJSON(w, http.StatusOK, titleResponse{Title: title, From: from})
}

// cleanTitle trims a title and refuses one that is too long or holds a
// control or format character, such as a bidi override that would make it
// read differently in a client that does not reveal it; it returns the
// title, or why it was refused.
func cleanTitle(raw string) (string, string) {
	t := strings.TrimSpace(raw)
	if !utf8.ValidString(t) {
		return "", "the title is not valid text"
	}
	if utf8.RuneCountInString(t) > maxTitleRunes {
		return "", "the title is longer than 120 characters"
	}
	if strings.IndexFunc(t, unicode.IsControl) >= 0 {
		return "", "the title holds a control character, such as a line break"
	}
	if hiddenFormat(t) {
		return "", "the title holds an invisible format character, such as a direction override"
	}
	return t, ""
}

// hiddenFormat reports a format character in t, other than a zero-width
// joiner between two emoji, which joins them into one (👨‍💻) and hides nothing.
func hiddenFormat(t string) bool {
	runes := []rune(t)
	emoji := func(r rune) bool { return unicode.Is(unicode.So, r) }
	for i, r := range runes {
		if !unicode.Is(unicode.Cf, r) {
			continue
		}
		joined := r == '\u200d' && i > 0 && i+1 < len(runes) && emoji(runes[i+1]) &&
			(emoji(runes[i-1]) || runes[i-1] == '\ufe0f' || runes[i-1] >= 0x1f3fb && runes[i-1] <= 0x1f3ff)
		if !joined {
			return true
		}
	}
	return false
}
