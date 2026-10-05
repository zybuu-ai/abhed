package server

import (
	"encoding/json"
	"net/http"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// exportSession hands the session's owner its record to keep: the transcript
// as a self-contained HTML page (the command line's /export page), or the
// events as recorded, one JSON object a line. Both are attachments, never
// drawn on this origin. The lines are the record as this store holds it: a
// store with no hash chain, such as Postgres, gives no chain to verify.
func (s *Server) exportSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validSessionID(id) || !s.readable(r, id, "export") {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	format := r.URL.Query().Get("format")
	if format != "html" && format != "jsonl" {
		WriteError(w, http.StatusBadRequest, "format is html or jsonl")
		return
	}
	events, err := s.store.Events(id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "the record could not be read")
		return
	}
	if len(events) == 0 {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(id+"."+format)+`"`)
	if format == "html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(agent.ExportHTML(id, events)))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return
		}
	}
}
