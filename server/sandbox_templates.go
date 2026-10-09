package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/zybuu-ai/abhed/controlplane"
)

// Sandbox templates (control-plane-contract.md §4.4): named defaults a
// sandbox is created from. Every caller of the tenant reads them; only an
// administrator of the whole tenant creates or removes one, or anyone on a
// server with no sign-in.

type templateList struct {
	Items []controlplane.Template `json:"items"`
	Next  string                  `json:"next,omitempty"`
}

// templateRequest is a create's body: what the caller may set.
type templateRequest struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Driver      string            `json:"driver,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Settings    map[string]string `json:"settings,omitempty"`
}

func (s *Server) templateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sandbox-templates", s.createTemplate)
	mux.HandleFunc("GET /v1/sandbox-templates", s.listTemplates)
	mux.HandleFunc("GET /v1/sandbox-templates/{name}", s.getTemplate)
	mux.HandleFunc("DELETE /v1/sandbox-templates/{name}", s.deleteTemplate)
}

func writeTemplate(w http.ResponseWriter, status int, t controlplane.Template) {
	w.Header().Set("ETag", controlplane.ETag(t.ResourceVersion))
	WriteJSON(w, status, t)
}

// templateWriter is the caller as one who may change templates: on a
// server with no sign-in, whoever reaches it administers it, as it does
// everything else there.
func (s *Server) templateWriter(c controlplane.Caller) controlplane.Caller {
	if !s.multiUser() && len(c.Scope) == 0 {
		c.Admin = true
	}
	return c
}

func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.sandboxCaller(w, r)
	if !ok || !cpLimit(s.cpMutations, w, c) {
		return
	}
	c = s.templateWriter(c)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		badCPBody(w, err)
		return
	}
	s.idempotentAny(w, r, c, body, func() (int, any, error) {
		var req templateRequest
		if err := decodeStrict(body, &req); err != nil {
			return 0, nil, err
		}
		t, err := s.opts.ControlPlane.CreateTemplate(r.Context(), c, controlplane.Template{Name: req.Name,
			Description: req.Description, Driver: req.Driver, Labels: req.Labels, Settings: req.Settings})
		return http.StatusCreated, t, err
	})
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.sandboxCaller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	sel, err := controlplane.ParseSelector(q.Get("selector"))
	if err != nil {
		s.failCP(w, r, err)
		return
	}
	o := controlplane.TemplateListOptions{Selector: sel, After: q.Get("after")}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			writeCPError(w, http.StatusBadRequest, "invalid", "limit must be a positive whole number")
			return
		}
		o.Limit = n
	}
	items, next, err := s.opts.ControlPlane.ListTemplates(r.Context(), c, o)
	if err != nil {
		s.failCP(w, r, err)
		return
	}
	if items == nil {
		items = []controlplane.Template{}
	}
	WriteJSON(w, http.StatusOK, templateList{Items: items, Next: next})
}

func (s *Server) getTemplate(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.sandboxCaller(w, r)
	if !ok {
		return
	}
	t, err := s.opts.ControlPlane.GetTemplate(r.Context(), c, r.PathValue("name"))
	if err != nil {
		s.failCP(w, r, err)
		return
	}
	writeTemplate(w, http.StatusOK, t)
}

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.sandboxCaller(w, r)
	if !ok || !cpLimit(s.cpMutations, w, c) {
		return
	}
	c = s.templateWriter(c)
	name := r.PathValue("name")
	s.idempotentAny(w, r, c, nil, func() (int, any, error) {
		err := s.opts.ControlPlane.DeleteTemplate(r.Context(), c, name, r.Header.Get("If-Match"))
		return http.StatusOK, map[string]string{"deleted": name}, err
	})
}

// eventsResult is a page of a sandbox's record.
type eventsResult struct {
	Items []controlplane.AuditEvent `json:"items"`
	// Next is the sequence number to pass as after for the next page.
	Next int64 `json:"next,omitempty"`
}

// sandboxEvents lists what the record holds for one sandbox, oldest first:
// GET /v1/sandboxes/{id}:events?after=SEQ&limit=N.
func (s *Server) sandboxEvents(w http.ResponseWriter, r *http.Request, call verbCall) {
	q := r.URL.Query()
	var o controlplane.EventsOptions
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeCPError(w, http.StatusBadRequest, "invalid", "after must be a whole number")
			return
		}
		o.After = n
	}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			writeCPError(w, http.StatusBadRequest, "invalid", "limit must be a positive whole number")
			return
		}
		o.Limit = n
	}
	items, next, err := s.opts.ControlPlane.Events(r.Context(), call.caller, call.id, o)
	if err != nil {
		s.failCP(w, r, err)
		return
	}
	if items == nil {
		items = []controlplane.AuditEvent{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(eventsResult{Items: items, Next: next})
}
