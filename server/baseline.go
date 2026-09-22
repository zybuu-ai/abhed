package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// maxSaveBytes bounds a file saved or accepted from the workbench. It is
// larger than the JSON cap because a file is the one body that is not small.
const maxSaveBytes = 4<<20 + 1<<20

type acceptRequest struct {
	Path string `json:"path"`
	// Content is the reviewed text the baseline moves to. It is what the
	// reviewer saw; the file on disk is not touched.
	Content string `json:"content"`
}

type originalResponse struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Existed bool   `json:"existed"`
	Binary  bool   `json:"binary"`
}

// originalFile returns what a changed file held before the session first
// changed it, for the review view to compare against.
func (s *Server) originalFile(w http.ResponseWriter, r *http.Request) {
	v, ok := s.view(w, r)
	if !ok {
		return
	}
	defer v.Close()
	rel, err := v.resolve(r.URL.Query().Get("path"))
	if err != nil {
		writeViewError(w, err)
		return
	}
	live, found := s.session(r.PathValue("id"), TenantOf(r.Context()), UserOf(r.Context()))
	if !found || live.undo == nil {
		WriteError(w, http.StatusNotFound, "no recorded change to this file")
		return
	}
	before, existed, known := live.undo.Original(filepath.Join(v.sess.Root, rel))
	if !known {
		WriteError(w, http.StatusNotFound, "no recorded change to this file")
		return
	}
	out := originalResponse{Path: filepath.ToSlash(rel), Existed: existed}
	if tools.IsBinary(before) {
		out.Binary = true
	} else {
		out.Content = string(before)
	}
	WriteJSON(w, http.StatusOK, out)
}

// acceptChange keeps a reviewed change: the file's baseline in the changes
// view moves to the content the reviewer accepted, and the record says so.
// Rejecting needs no endpoint: it is a save of the earlier text, which goes
// through the same call as any other save and is recorded as one.
func (s *Server) acceptChange(w http.ResponseWriter, r *http.Request) {
	live, _, ok := s.manualSession(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSaveBytes)
	var req acceptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "the request is malformed or too large")
		return
	}
	v, err := s.openView()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "workspace is not readable")
		return
	}
	defer v.Close()
	rel, err := v.resolve(req.Path)
	if err != nil {
		writeViewError(w, err)
		return
	}
	abs := filepath.Join(v.sess.Root, rel)
	if live.undo == nil || !live.undo.Accept(abs, []byte(req.Content)) {
		WriteError(w, http.StatusNotFound, "no recorded change to this file")
		return
	}
	if _, err := live.Loop.Recorder.Record(agent.EvChangeAccepted, agent.ActorUser, agent.Trusted,
		map[string]any{"path": filepath.ToSlash(rel), "bytes": len(req.Content)}); err != nil {
		WriteError(w, http.StatusInternalServerError, "the acceptance could not be recorded")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
