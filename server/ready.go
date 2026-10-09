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

// ready answers whether this server should be sent work now: it is not
// draining, its secrets store reads, its database answers and, when the
// control plane is on, its reconciler runs. /v1/health says the process is
// up; this says it can serve. Each check is named with ok or failed only,
// since the route is public.
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
