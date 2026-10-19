package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
)

// APIAuditPrefix starts the action of every API audit event, which is the
// route pattern after it: "api:POST /v1/sessions/{id}/messages".
const APIAuditPrefix = "api:"

// Mutating reports whether a request of this method changes something, and
// so is written to the audit record before and after it runs.
func Mutating(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	}
	return true
}

// unaudited are the mutating routes the API audit leaves out, each with why.
// The route-table test holds every other mutating route to an event.
var unaudited = map[string]string{
	"POST /v1/streams/{mux}/{ch}":   "opens a channel for a read-only stream on a shared connection",
	"DELETE /v1/streams/{mux}/{ch}": "closes a read-only stream's channel",
}

// routed wraps the finished mux: each request is counted by its route
// pattern, and a mutating one is written to the API audit record with its
// principal before it runs and again with its outcome. A request whose
// first event cannot be written is refused, so no change goes unrecorded.
func (s *Server) routed(mux *http.ServeMux) http.Handler {
	audit, m := s.opts.APIAudit, s.opts.Metrics
	if audit == nil && m == nil {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		_, pattern := mux.Handler(r)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		var ev controlplane.AuditEvent
		audited := audit != nil && pattern != "" && Mutating(r.Method) && unaudited[pattern] == ""
		if audited {
			ev = apiEvent(r, pattern, s.opts.Config.Server.TrustProxy)
			if err := s.appendAudit(r.Context(), ev); err != nil {
				s.log.Error("api audit: the request was refused, as its record could not be written",
					"route", pattern, "user", ev.Principal, "err", err)
				WriteError(rec, http.StatusServiceUnavailable, "the audit record could not be written, so nothing was changed")
				if m != nil {
					m.Request(pattern, r.Method, rec.status, time.Since(start))
				}
				return
			}
		}
		done := false
		defer func() {
			status := rec.status
			if !done {
				status = http.StatusInternalServerError // a panic, which withMiddleware answers
			}
			if audited {
				out := ev
				out.At, out.Outcome = time.Now().UTC(), controlplane.OutcomeOK
				if status >= 400 {
					out.Outcome, out.Code = controlplane.OutcomeFailed, strconv.Itoa(status)
				}
				// The change has been made: its outcome is written though the caller has gone.
				if err := s.appendAudit(context.WithoutCancel(r.Context()), out); err != nil {
					s.log.Error("api audit: the outcome could not be written", "route", pattern, "user", ev.Principal, "err", err)
				}
			}
			if m != nil {
				m.Request(pattern, r.Method, status, time.Since(start))
			}
		}()
		mux.ServeHTTP(rec, r)
		done = true
	})
}

// appendAudit writes one API audit event and counts the write.
func (s *Server) appendAudit(ctx context.Context, ev controlplane.AuditEvent) error {
	start := time.Now()
	err := s.opts.APIAudit.Append(ctx, ev)
	if m := s.opts.Metrics; m != nil {
		m.Audit(ev.Action, ev.Outcome, false, time.Since(start), err)
	}
	return err
}

// apiEvent is the requested event of a mutating request: who, which route,
// and the values its wildcards took. Never the body or the query.
func apiEvent(r *http.Request, pattern string, trustProxy bool) controlplane.AuditEvent {
	detail := map[string]string{"method": r.Method, "route": pattern, "tenant": TenantOf(r.Context())}
	if ip := clientIP(r, trustProxy); ip != "" {
		detail["remote"] = ip
	}
	for k, v := range wildcards(pattern, r.URL.EscapedPath()) {
		detail["path."+k] = v
	}
	return controlplane.AuditEvent{
		At:        time.Now().UTC(),
		Principal: UserOf(r.Context()),
		Action:    APIAuditPrefix + pattern,
		Outcome:   controlplane.OutcomeRequested,
		Detail:    detail,
	}
}

// maxWildcard is the longest wildcard value an audit event keeps.
const maxWildcard = 200

// wildcards matches a pattern's path against a request path and returns
// what each named wildcard took.
func wildcards(pattern, path string) map[string]string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = pattern[i+1:]
	}
	if i := strings.IndexByte(pattern, '/'); i > 0 {
		pattern = pattern[i:] // a host in the pattern
	}
	ps, us := strings.Split(pattern, "/"), strings.Split(path, "/")
	out := map[string]string{}
	for i, seg := range ps {
		if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") || i >= len(us) {
			continue
		}
		name, rest := strings.CutSuffix(seg[1:len(seg)-1], "...")
		if name == "$" {
			continue
		}
		v := us[i]
		if rest {
			v = strings.Join(us[i:], "/")
		}
		if len(v) > maxWildcard {
			v = v[:maxWildcard]
		}
		out[name] = v
	}
	return out
}
