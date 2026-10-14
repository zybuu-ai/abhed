package server

import (
	"context"
	"net/http"
	"time"
)

// pinger is an event store that can say whether its database answers.
type pinger interface {
	Ping(ctx context.Context) error
}

// readyTimeout bounds the checks one readiness probe makes.
const readyTimeout = 3 * time.Second

// ready says whether this server can serve now, where /v1/health says it is
// up. The route is public, so each check reports only ok or failed.
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	checks := map[string]string{}
	pass := func(name string, ok bool) {
		checks[name] = "ok"
		if !ok {
			checks[name] = "failed"
		}
	}
	pass("serving", !s.draining.Load())
	if l, ok := s.opts.Redact.(interface{ Loads() error }); ok {
		pass("secrets", l.Loads() == nil)
	}
	if p, ok := s.under().(pinger); ok {
		pass("event_store", p.Ping(ctx) == nil)
	}
	if cp := s.opts.ControlPlane; cp != nil {
		pass("control_plane", cp.Ready(ctx) == nil)
	}
	ready := true
	for _, v := range checks {
		ready = ready && v == "ok"
	}
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	WriteJSON(w, status, map[string]any{"ready": ready, "checks": checks})
}
