package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/server/expose"
)

// The verbs backed by a driver's optional capabilities (contract §4). A
// driver without the capability answers 501 unsupported, after the usual
// visibility check, and nothing is recorded.

// snapshotResult is a :snapshot's answer.
type snapshotResult struct {
	Snapshot   driver.SnapshotRef `json:"snapshot"`
	Generation int64              `json:"generation"`
}

func (s *Server) snapshotSandbox(w http.ResponseWriter, r *http.Request, call verbCall) {
	if !cpLimit(s.cpMutations, w, call.caller) {
		return
	}
	s.idempotentAny(w, r, call.caller, call.body, func() (int, any, error) {
		var req struct {
			Name string `json:"name"`
		}
		if err := decodeStrict(call.body, &req); err != nil {
			return 0, nil, err
		}
		ref, sb, err := s.opts.ControlPlane.Snapshot(r.Context(), call.caller, call.id, r.Header.Get("If-Match"), req.Name)
		return http.StatusCreated, snapshotResult{Snapshot: ref, Generation: sb.Generation}, err
	})
}

func (s *Server) restoreSandbox(w http.ResponseWriter, r *http.Request, call verbCall) {
	if !cpLimit(s.cpMutations, w, call.caller) {
		return
	}
	s.idempotent(w, r, call.caller, call.body, func() (int, controlplane.Sandbox, error) {
		var req struct {
			Snapshot *driver.SnapshotRef `json:"snapshot"`
		}
		if err := decodeStrict(call.body, &req); err != nil {
			return 0, controlplane.Sandbox{}, err
		}
		if req.Snapshot == nil {
			return 0, controlplane.Sandbox{}, fmt.Errorf("%w: snapshot is required, as :snapshot returned it", controlplane.ErrInvalid)
		}
		sb, err := s.opts.ControlPlane.Restore(r.Context(), call.caller, call.id, r.Header.Get("If-Match"), *req.Snapshot)
		return http.StatusAccepted, sb, err
	})
}

// copyRequest is a :cp's direction and path, from the query or, for a copy
// out, a JSON body.
type copyRequest struct {
	Direction string `json:"direction"`
	Path      string `json:"path"`
}

// cpSandbox copies into a sandbox (?direction=in&path=…, a tar body) or out
// of it (a tar response). A copy out that fails once its bytes have started
// says so in the X-Abhed-Error trailer.
func (s *Server) cpSandbox(w http.ResponseWriter, r *http.Request, call verbCall) {
	if !cpLimit(s.cpMutations, w, call.caller) {
		return
	}
	q := r.URL.Query()
	req := copyRequest{Direction: q.Get("direction"), Path: q.Get("path")}
	if req.Direction == "" {
		if err := decodeStrict(call.body, &req); err != nil {
			s.failCP(w, r, err)
			return
		}
	}
	if req.Path == "" {
		s.failCP(w, r, fmt.Errorf("%w: path is required", controlplane.ErrInvalid))
		return
	}
	switch req.Direction {
	case "in":
		sb, err := s.opts.ControlPlane.CopyIn(r.Context(), call.caller, call.id, req.Path, bytes.NewReader(call.body))
		if err != nil {
			s.failCP(w, r, err)
			return
		}
		w.Header().Set("X-Abhed-Generation", strconv.FormatInt(sb.Generation, 10))
		w.WriteHeader(http.StatusOK)
	case "out":
		started := false
		_, err := s.opts.ControlPlane.CopyOut(r.Context(), call.caller, call.id, req.Path, w, func(sb controlplane.Sandbox) {
			started = true
			w.Header().Set("Content-Type", "application/x-tar")
			w.Header().Set("X-Abhed-Generation", strconv.FormatInt(sb.Generation, 10))
			w.Header().Set("Trailer", "X-Abhed-Error")
			w.WriteHeader(http.StatusOK)
		})
		switch {
		case err != nil && !started:
			s.failCP(w, r, err)
		case err != nil:
			_, code := cpStatus(err)
			w.Header().Set("X-Abhed-Error", code+": "+err.Error())
		}
	default:
		s.failCP(w, r, fmt.Errorf("%w: direction must be in or out", controlplane.ErrInvalid))
	}
}

// exposeResult is an :expose's answer.
type exposeResult struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Path      string    `json:"path"`
	ExpiresAt time.Time `json:"expires_at"`
}

// exposeSandbox publishes a port of a Ready sandbox on an authenticated
// path for the caller alone. Off unless the operator turned exposing on.
func (s *Server) exposeSandbox(w http.ResponseWriter, r *http.Request, call verbCall) {
	if !cpLimit(s.cpMutations, w, call.caller) {
		return
	}
	ex := s.opts.Expose
	if ex == nil || !ex.Enabled() {
		if _, err := s.opts.ControlPlane.Get(r.Context(), call.caller, call.id); err != nil {
			s.failCP(w, r, err)
			return
		}
		writeCPError(w, http.StatusNotImplemented, "unsupported", "exposing sandbox services is off; the server's operator turns it on")
		return
	}
	who, ok := auth.FromContext(r.Context())
	if !ok || who == nil || auth.OwnsNothing(who.Owner()) {
		writeCPError(w, http.StatusForbidden, "forbidden", "an exposure needs a signed-in caller")
		return
	}
	s.idempotentAny(w, r, call.caller, call.body, func() (int, any, error) {
		var req struct {
			Port       int `json:"port"`
			TTLSeconds int `json:"ttl_seconds"`
		}
		if err := decodeStrict(call.body, &req); err != nil {
			return 0, nil, err
		}
		var e expose.Exposure
		_, err := s.opts.ControlPlane.UseDriver(r.Context(), call.caller, call.id, "expose", driver.CapForward,
			map[string]string{"port": strconv.Itoa(req.Port), "ttl_seconds": strconv.Itoa(req.TTLSeconds)},
			func(d driver.Driver, sb controlplane.Sandbox) error {
				var err error
				e, err = ex.Expose(r.Context(), expose.Request{Driver: d, SandboxID: sb.ID, Generation: sb.Generation,
					Port: req.Port, TTL: time.Duration(req.TTLSeconds) * time.Second, Owner: who.Owner(), Tenant: who.Tenant})
				return err
			})
		if err != nil {
			return 0, nil, err
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		return http.StatusCreated, exposeResult{ID: e.ID, URL: scheme + "://" + r.Host + e.Path, Path: e.Path, ExpiresAt: e.ExpiresAt}, nil
	})
}

// idempotentAny is idempotent for a verb whose answer is not a sandbox.
func (s *Server) idempotentAny(w http.ResponseWriter, r *http.Request, c controlplane.Caller, body []byte,
	op func() (int, any, error)) {
	idem := controlplane.Idempotency{Store: s.opts.ControlPlane.Store()}
	resp, replayed, err := idem.Do(r.Context(), c.Principal, r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"), body,
		func() (controlplane.Response, error) {
			status, v, err := op()
			if err != nil {
				return controlplane.Response{}, err
			}
			b, err := json.Marshal(v)
			if err != nil {
				return controlplane.Response{}, errors.Join(controlplane.ErrUnavailable, err)
			}
			return controlplane.Response{Status: status, Body: b}, nil
		})
	if err != nil {
		s.failCP(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}
