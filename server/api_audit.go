package server

import (
	"context"
	"encoding/json"
	"maps"
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

// exposureRoute reports whether pattern is an exposed service's: traffic to
// a person's own app in a sandbox, not a change to the control plane. The
// exposure itself is made and removed through audited sandbox routes; the
// app's requests, and the paths and tokens they carry, are kept out.
func (s *Server) exposureRoute(pattern string) bool {
	if s.opts.Expose == nil {
		return false
	}
	_, path, _ := strings.Cut(pattern, " ")
	return path == s.opts.Expose.Prefix()
}

// batched are routes a client calls for every few keys: each request gets
// its first event alone, so a refused write still stops it, and no outcome.
// A shell's lines are in the session record as terminal.input; a program's
// input is not recorded anywhere, as it may be a password typed with echo off.
var batched = map[string]string{
	"POST /v1/sessions/{id}/pty/{pty}/input": "a batch of keys for a terminal",
	"POST /v1/sessions/{id}/lsp/{lsp}":       "a message to a language server, sent as the file is edited",
}

// routed wraps the finished mux: each request is counted by its route
// pattern, and a mutating one is written to the API audit record with its
// principal before it runs and again with its outcome. A request whose
// first event cannot be written is refused, so no change goes unrecorded.
func (s *Server) routed(mux *http.ServeMux) http.Handler {
	audit, m := s.opts.APIAudit, s.opts.Metrics.abhed()
	if audit == nil && m == nil {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		_, pattern := mux.Handler(r)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		var ev controlplane.AuditEvent
		audited := audit != nil && pattern != "" && Mutating(r.Method) && unaudited[pattern] == "" && !s.exposureRoute(pattern)
		var created *createdSniffer
		var out http.ResponseWriter = rec
		if audited {
			ev = apiEvent(r, pattern, s.opts.Config.Server.TrustProxy)
			if batched[pattern] != "" {
				ev.Detail["batch"] = "one event a request, with no outcome"
			}
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
		if audited && r.Method == http.MethodPost && batched[pattern] == "" {
			created = &createdSniffer{statusRecorder: rec}
			out = created
		}
		done := false
		defer func() {
			status := rec.status
			if !done {
				status = http.StatusInternalServerError // a panic, which withMiddleware answers
			}
			if audited && batched[pattern] == "" {
				out := ev
				out.Detail = maps.Clone(ev.Detail)
				out.At, out.Outcome = time.Now().UTC(), controlplane.OutcomeOK
				if status >= 400 {
					out.Outcome, out.Code = controlplane.OutcomeFailed, strconv.Itoa(status)
				} else if created != nil {
					for k, v := range created.ids() {
						out.Detail["created."+k] = v
					}
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
		mux.ServeHTTP(out, r)
		done = true
	})
}

// appendAudit writes one API audit event and counts the write.
func (s *Server) appendAudit(ctx context.Context, ev controlplane.AuditEvent) error {
	start := time.Now()
	err := s.opts.APIAudit.Append(ctx, ev)
	if m := s.opts.Metrics.abhed(); m != nil {
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

// createdSniffer keeps the start of a POST's JSON answer, so the outcome
// event can name what the request created. Only id fields are kept from it.
type createdSniffer struct {
	*statusRecorder
	buf []byte
}

// maxSniff is how much of an answer is kept to find the ids in.
const maxSniff = 8 << 10

func (c *createdSniffer) Write(b []byte) (int, error) {
	if room := maxSniff - len(c.buf); room > 0 {
		c.buf = append(c.buf, b[:min(room, len(b))]...)
	}
	return c.statusRecorder.Write(b)
}

// createdFields are the answer's fields that name a created resource.
var createdFields = []string{"id", "session_id", "name"}

// ids are the created resource's ids in a 2xx JSON answer, each a short
// token; an answer that is not one, or a value that is not, gives nothing.
func (c *createdSniffer) ids() map[string]string {
	if c.status < 200 || c.status > 299 || !strings.HasPrefix(c.Header().Get("Content-Type"), "application/json") {
		return nil
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(c.buf, &top) != nil {
		return nil
	}
	out := map[string]string{}
	for _, f := range createdFields {
		var v string
		if raw, ok := top[f]; ok && json.Unmarshal(raw, &v) == nil && idToken(v) {
			out[f] = v
		}
	}
	return out
}

// idToken reports whether v looks like an identifier rather than text.
func idToken(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	for _, c := range v {
		ok := c == '_' || c == '-' || c == '.' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
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
