package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
)

// The control plane's REST surface (control-plane-contract.md §4); the
// recording and lifecycle live in controlplane.Service, the HTTP here.

// SandboxRouter sends a call for a sandbox another replica owns to that
// replica (Enterprise HA). The request is already authenticated here, and
// the owner authenticates it again.
type SandboxRouter interface {
	// Route answers the request itself, by relaying or refusing it, and
	// reports true; false means this process handles it.
	Route(w http.ResponseWriter, r *http.Request, id string) bool
}

// sandboxVerb is one custom method, POST or GET /v1/sandboxes/{id}:verb.
// A verb with ws also takes a GET that upgrades to a WebSocket.
type sandboxVerb struct {
	method string
	handle func(s *Server, w http.ResponseWriter, r *http.Request, call verbCall)
	ws     func(s *Server, w http.ResponseWriter, r *http.Request, call verbCall)
}

// verbCall is what a verb handler is given once the caller is known.
type verbCall struct {
	id     string
	verb   string
	caller controlplane.Caller
	body   []byte
}

// sandboxVerbs is the routing seam. :attach and :forward are WebSockets
// only; :exec and :ssh also take a WebSocket GET (exec with stdin, and the
// SSH relay).
var sandboxVerbs = map[string]sandboxVerb{
	"start":    {method: http.MethodPost, handle: (*Server).startSandbox},
	"stop":     {method: http.MethodPost, handle: (*Server).stopSandbox},
	"exec":     {method: http.MethodPost, handle: (*Server).execSandbox, ws: (*Server).execStream},
	"attach":   {method: http.MethodGet, ws: (*Server).attachSandbox},
	"ssh":      {method: http.MethodPost, handle: (*Server).sshInfo, ws: (*Server).sshRelay},
	"forward":  {method: http.MethodGet, ws: (*Server).forwardSandbox},
	"expose":   {method: http.MethodPost, handle: (*Server).exposeSandbox},
	"cp":       {method: http.MethodPost, handle: (*Server).cpSandbox},
	"snapshot": {method: http.MethodPost, handle: (*Server).snapshotSandbox},
	"restore":  {method: http.MethodPost, handle: (*Server).restoreSandbox},
}

// cpError is the contract's error body.
type cpError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeCPError(w http.ResponseWriter, status int, code, msg string) {
	var e cpError
	e.Error.Code, e.Error.Message = code, msg
	WriteJSON(w, status, e)
}

// cpStatus maps an error from the control plane to its status and code.
func cpStatus(err error) (int, string) {
	code := controlplane.ErrorCode(err)
	switch {
	case errors.Is(err, controlplane.ErrIdempotencyMismatch):
		return http.StatusUnprocessableEntity, "invalid"
	case errors.Is(err, driver.ErrOutputLimit):
		return http.StatusRequestEntityTooLarge, "invalid"
	}
	switch code {
	case "not_found":
		return http.StatusNotFound, code
	case "forbidden", "approval_required", "quota_exceeded":
		return http.StatusForbidden, code
	case "invalid":
		return http.StatusBadRequest, code
	case "conflict":
		return http.StatusConflict, code
	case "precondition_failed":
		return http.StatusPreconditionFailed, code
	case "unsupported":
		return http.StatusNotImplemented, code
	case "unavailable":
		return http.StatusServiceUnavailable, code
	}
	return http.StatusInternalServerError, "internal"
}

func (s *Server) failCP(w http.ResponseWriter, r *http.Request, err error) {
	status, code := cpStatus(err)
	if need := new(controlplane.ApprovalNeeded); errors.As(err, &need) {
		w.Header().Set(approvalHeader, need.ID)
	}
	msg := err.Error()
	if code == "internal" {
		// Fails closed, and says nothing about why to the caller.
		s.log.Error("sandbox request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		msg = "internal error"
	}
	writeCPError(w, status, code, msg)
}

// cpTenant is the one tenant this server's control plane serves.
func (s *Server) cpTenant() string {
	if t := s.opts.Config.Storage.Tenant; t != "" {
		return t
	}
	return "default"
}

// approvalHeader names the approval a request is sent again with, and the
// one a refusal asks to be approved.
const approvalHeader = "Abhed-Approval"

// sandboxCaller resolves who is asking, or answers and returns false. The
// store is opened for one tenant, so a request in another is refused. The
// returned request carries the approval the caller named, for the guard.
func (s *Server) sandboxCaller(w http.ResponseWriter, r *http.Request) (controlplane.Caller, *http.Request, bool) {
	if TenantOf(r.Context()) != s.cpTenant() {
		writeCPError(w, http.StatusForbidden, "forbidden", "sandboxes on this server belong to another tenant")
		return controlplane.Caller{}, r, false
	}
	c := controlplane.Caller{Principal: UserOf(r.Context()), Admin: s.IsAdmin(r)}
	if s.opts.SandboxCaller != nil {
		var err error
		if c, err = s.opts.SandboxCaller(r, c); err != nil {
			s.failCP(w, r, err)
			return controlplane.Caller{}, r, false
		}
	}
	return c, r.WithContext(controlplane.WithApproval(r.Context(), r.Header.Get(approvalHeader))), true
}

// cpRateWindow is the window the per-minute settings count in.
var cpRateWindow = time.Minute

// cpLimit applies a per-caller rate limit, answering 429 when it is spent.
func cpLimit(l *limiter, w http.ResponseWriter, c controlplane.Caller) bool {
	if l == nil || l.allow(c.Principal) {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(l.retryAfter(c.Principal).Seconds())+1))
	writeCPError(w, http.StatusTooManyRequests, "unavailable", "too many requests; try again later")
	return false
}

func (s *Server) sandboxRoutes(mux *http.ServeMux) {
	if s.opts.ControlPlane == nil {
		return
	}
	mux.HandleFunc("POST /v1/sandboxes", s.createSandbox)
	mux.HandleFunc("GET /v1/sandboxes", s.listSandboxes)
	// One segment may not mix a wildcard and a literal, so a verb arrives
	// in the same wildcard as the id and is split off at its last ':'.
	mux.HandleFunc("GET /v1/sandboxes/{target}", s.sandboxTarget)
	mux.HandleFunc("POST /v1/sandboxes/{target}", s.sandboxTarget)
	mux.HandleFunc("PATCH /v1/sandboxes/{target}", s.sandboxTarget)
	mux.HandleFunc("DELETE /v1/sandboxes/{target}", s.sandboxTarget)
	// Exposed services, behind the same sign-in; each answers its owner only.
	// Registered per method: a method-less pattern would clash with "GET /".
	if s.opts.Expose != nil {
		for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
			mux.Handle(m+" "+s.opts.Expose.Prefix(), s.opts.Expose)
		}
	}
}

// splitTarget splits "sbx_…:verb" at its last ':'.
func splitTarget(target string) (id, verb string) {
	if i := strings.LastIndexByte(target, ':'); i >= 0 {
		return target[:i], target[i+1:]
	}
	return target, ""
}

func (s *Server) sandboxTarget(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.sandboxCaller(w, r)
	if !ok {
		return
	}
	id, verb := splitTarget(r.PathValue("target"))
	if !controlplane.ValidID(id) {
		writeCPError(w, http.StatusNotFound, "not_found", "no sandbox "+id)
		return
	}
	// A call that acts on the sandbox goes to the replica that owns it; a
	// plain read is answered here from the shared store.
	if rt := s.opts.SandboxRouter; rt != nil && (verb != "" || r.Method != http.MethodGet) && rt.Route(w, r, id) {
		return
	}
	if verb == "" {
		switch r.Method {
		case http.MethodGet:
			s.getSandbox(w, r, c, id)
		case http.MethodPatch:
			s.patchSandbox(w, r, c, id)
		case http.MethodDelete:
			s.deleteSandbox(w, r, c, id)
		default:
			writeCPError(w, http.StatusMethodNotAllowed, "invalid", r.Method+" is not a method on a sandbox; use a verb")
		}
		return
	}
	v, known := sandboxVerbs[verb]
	if !known {
		writeCPError(w, http.StatusNotFound, "not_found", "no verb :"+verb)
		return
	}
	if v.ws != nil && r.Method == http.MethodGet {
		v.ws(s, w, r, verbCall{id: id, verb: verb, caller: c})
		return
	}
	if r.Method != v.method {
		allow := v.method
		if v.ws != nil && v.method != http.MethodGet {
			allow += ", GET"
		}
		w.Header().Set("Allow", allow)
		writeCPError(w, http.StatusMethodNotAllowed, "invalid", ":"+verb+" takes "+allow)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		badCPBody(w, err)
		return
	}
	v.handle(s, w, r, verbCall{id: id, verb: verb, caller: c, body: body})
}

func badCPBody(w http.ResponseWriter, err error) {
	if tooBig := new(http.MaxBytesError); errors.As(err, &tooBig) {
		writeCPError(w, http.StatusRequestEntityTooLarge, "invalid", fmt.Sprintf("the request body is larger than %d bytes", tooBig.Limit))
		return
	}
	writeCPError(w, http.StatusBadRequest, "invalid", "the request body could not be read")
}

// decodeStrict decodes body into v, refusing unknown fields; an empty
// body leaves v as it is.
func decodeStrict(body []byte, v any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: the body is not valid JSON for this call: %w", controlplane.ErrInvalid, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: the body holds more than one JSON value", controlplane.ErrInvalid)
	}
	return nil
}

// writeSandbox answers with a sandbox and its ETag.
func writeSandbox(w http.ResponseWriter, status int, sb controlplane.Sandbox) {
	w.Header().Set("ETag", controlplane.ETag(sb.ResourceVersion))
	WriteJSON(w, status, sb)
}

// idempotent runs a mutation once per Idempotency-Key. op errs only when it
// changed nothing; its response is what a repeat gets.
func (s *Server) idempotent(w http.ResponseWriter, r *http.Request, c controlplane.Caller, body []byte,
	op func() (int, controlplane.Sandbox, error)) {
	idem := controlplane.Idempotency{Store: s.opts.ControlPlane.Store()}
	resp, replayed, err := idem.Do(r.Context(), c.Principal, r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"), body,
		func() (controlplane.Response, error) {
			status, sb, err := op()
			if err != nil {
				return controlplane.Response{}, err
			}
			b, err := json.Marshal(sb)
			if err != nil {
				return controlplane.Response{}, err
			}
			return controlplane.Response{Status: status, Body: b}, nil
		})
	if err != nil {
		s.failCP(w, r, err)
		return
	}
	var sb controlplane.Sandbox
	if json.Unmarshal(resp.Body, &sb) == nil && sb.ResourceVersion > 0 {
		w.Header().Set("ETag", controlplane.ETag(sb.ResourceVersion))
	}
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.sandboxCaller(w, r)
	if !ok || !cpLimit(s.cpMutations, w, c) {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		badCPBody(w, err)
		return
	}
	s.idempotent(w, r, c, body, func() (int, controlplane.Sandbox, error) {
		var req controlplane.CreateRequest
		if err := decodeStrict(body, &req); err != nil {
			return 0, controlplane.Sandbox{}, err
		}
		sb, err := s.opts.ControlPlane.Create(r.Context(), c, req)
		return http.StatusCreated, sb, err
	})
}

type sandboxList struct {
	Items []controlplane.Sandbox `json:"items"`
	Next  string                 `json:"next,omitempty"`
}

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
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
	o := controlplane.ListOptions{Selector: sel, State: controlplane.State(q.Get("state")), After: q.Get("after")}
	if o.State != "" && !o.State.Valid() {
		writeCPError(w, http.StatusBadRequest, "invalid", "unknown state "+strconv.Quote(q.Get("state")))
		return
	}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			writeCPError(w, http.StatusBadRequest, "invalid", "limit must be a positive whole number")
			return
		}
		o.Limit = n
	}
	items, next, err := s.opts.ControlPlane.List(r.Context(), c, o)
	if err != nil {
		s.failCP(w, r, err)
		return
	}
	if items == nil {
		items = []controlplane.Sandbox{}
	}
	WriteJSON(w, http.StatusOK, sandboxList{Items: items, Next: next})
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request, c controlplane.Caller, id string) {
	sb, err := s.opts.ControlPlane.Get(r.Context(), c, id)
	if err != nil {
		s.failCP(w, r, err)
		return
	}
	writeSandbox(w, http.StatusOK, sb)
}

// parsePatch reads a JSON merge patch of labels, template and settings.
func parsePatch(body []byte) (controlplane.Patch, error) {
	var p controlplane.Patch
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return p, fmt.Errorf("%w: the body must be a JSON merge patch object", controlplane.ErrInvalid)
	}
	for k, raw := range top {
		null := string(bytes.TrimSpace(raw)) == "null"
		switch k {
		case "labels":
			if null {
				p.ClearLabels = true
				continue
			}
			if err := json.Unmarshal(raw, &p.Labels); err != nil {
				return p, fmt.Errorf("%w: labels must map names to strings or null", controlplane.ErrInvalid)
			}
		case "settings":
			if null {
				p.ClearSettings = true
				continue
			}
			if err := json.Unmarshal(raw, &p.Settings); err != nil {
				return p, fmt.Errorf("%w: settings must map names to strings or null", controlplane.ErrInvalid)
			}
		case "template":
			var t string
			if null || json.Unmarshal(raw, &t) != nil {
				return p, fmt.Errorf("%w: template must be a string", controlplane.ErrInvalid)
			}
			p.Template = &t
		default:
			return p, fmt.Errorf("%w: %s cannot be changed with PATCH", controlplane.ErrInvalid, k)
		}
	}
	return p, nil
}

func (s *Server) patchSandbox(w http.ResponseWriter, r *http.Request, c controlplane.Caller, id string) {
	if !cpLimit(s.cpMutations, w, c) {
		return
	}
	ifMatch := r.Header.Get("If-Match")
	if strings.TrimSpace(ifMatch) == "" {
		writeCPError(w, http.StatusPreconditionRequired, "precondition_failed", "PATCH needs If-Match with the sandbox's ETag")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		badCPBody(w, err)
		return
	}
	s.idempotent(w, r, c, body, func() (int, controlplane.Sandbox, error) {
		p, err := parsePatch(body)
		if err != nil {
			return 0, controlplane.Sandbox{}, err
		}
		sb, err := s.opts.ControlPlane.Update(r.Context(), c, id, ifMatch, p)
		return http.StatusOK, sb, err
	})
}

func (s *Server) deleteSandbox(w http.ResponseWriter, r *http.Request, c controlplane.Caller, id string) {
	if !cpLimit(s.cpMutations, w, c) {
		return
	}
	s.idempotent(w, r, c, nil, func() (int, controlplane.Sandbox, error) {
		sb, err := s.opts.ControlPlane.Delete(r.Context(), c, id, r.Header.Get("If-Match"))
		return http.StatusAccepted, sb, err
	})
}

func (s *Server) startSandbox(w http.ResponseWriter, r *http.Request, call verbCall) {
	if !cpLimit(s.cpMutations, w, call.caller) {
		return
	}
	s.idempotent(w, r, call.caller, call.body, func() (int, controlplane.Sandbox, error) {
		var req struct{}
		if err := decodeStrict(call.body, &req); err != nil {
			return 0, controlplane.Sandbox{}, err
		}
		sb, err := s.opts.ControlPlane.Start(r.Context(), call.caller, call.id, r.Header.Get("If-Match"))
		return http.StatusAccepted, sb, err
	})
}

func (s *Server) stopSandbox(w http.ResponseWriter, r *http.Request, call verbCall) {
	if !cpLimit(s.cpMutations, w, call.caller) {
		return
	}
	s.idempotent(w, r, call.caller, call.body, func() (int, controlplane.Sandbox, error) {
		var req struct {
			GraceSeconds *int `json:"grace_seconds"`
		}
		if err := decodeStrict(call.body, &req); err != nil {
			return 0, controlplane.Sandbox{}, err
		}
		grace := 10 * time.Second
		if req.GraceSeconds != nil {
			grace = time.Duration(*req.GraceSeconds) * time.Second
		}
		sb, err := s.opts.ControlPlane.Stop(r.Context(), call.caller, call.id, r.Header.Get("If-Match"), grace)
		return http.StatusAccepted, sb, err
	})
}

// execFrame is one line of an :exec stream. Byte payloads are base64.
type execFrame struct {
	Stdout []byte   `json:"stdout,omitempty"`
	Stderr []byte   `json:"stderr,omitempty"`
	Exit   *int     `json:"exit,omitempty"`
	Error  *cpFault `json:"error,omitempty"`
}

type cpFault struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// maxExecFrame bounds the bytes one frame carries, as the driver protocol does.
const maxExecFrame = 32 << 10

// frameWriter turns writes into NDJSON frames on one response, one writer
// at a time.
type frameWriter struct {
	mu  sync.Mutex
	w   http.ResponseWriter
	enc *json.Encoder
}

func (f *frameWriter) frame(fr execFrame) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enc.Encode(fr); err != nil {
		return err
	}
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return nil
}

type streamOut struct {
	f      *frameWriter
	stderr bool
}

func (o streamOut) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += maxExecFrame {
		chunk := p[off:min(off+maxExecFrame, len(p))]
		fr := execFrame{Stdout: chunk}
		if o.stderr {
			fr = execFrame{Stderr: chunk}
		}
		if err := o.f.frame(fr); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

// execSandbox streams one command's output as NDJSON frames, ending with
// {"exit": n} or, when it did not run to an exit code, {"error": …}.
func (s *Server) execSandbox(w http.ResponseWriter, r *http.Request, call verbCall) {
	if !cpLimit(s.cpExec, w, call.caller) {
		return
	}
	var req controlplane.ExecRequest
	if err := decodeStrict(call.body, &req); err != nil {
		s.failCP(w, r, err)
		return
	}
	if req.Stdin {
		if _, err := s.opts.ControlPlane.Get(r.Context(), call.caller, call.id); err != nil {
			s.failCP(w, r, err)
			return
		}
		writeCPError(w, http.StatusBadRequest, "invalid", ":exec with stdin is a WebSocket: GET the same path with an upgrade to "+protoExec)
		return
	}
	pol := s.sandboxPolicy(call.id)
	fw := &frameWriter{w: w, enc: json.NewEncoder(w)}
	started := false
	idem := controlplane.Idempotency{Store: s.opts.ControlPlane.Store()}
	resp, replayed, err := idem.Do(r.Context(), call.caller.Principal, r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"), call.body,
		func() (controlplane.Response, error) {
			res, _, err := s.opts.ControlPlane.Exec(r.Context(), call.caller, call.id, r.Header.Get("If-Match"), req, controlplane.ExecIO{
				Stdout: streamOut{f: fw}, Stderr: streamOut{f: fw, stderr: true},
				Screen: func(command string) error { return denied(pol, command, false) },
				Started: func(sb controlplane.Sandbox) {
					started = true
					w.Header().Set("Content-Type", "application/x-ndjson")
					// What anything bound to this run is checked against later.
					w.Header().Set("X-Abhed-Generation", strconv.FormatInt(sb.Generation, 10))
					w.WriteHeader(http.StatusOK)
					if fl, ok := w.(http.Flusher); ok {
						fl.Flush()
					}
				}})
			if !started {
				if err == nil {
					err = errors.New("exec ended without running")
				}
				return controlplane.Response{}, err
			}
			last := execFrame{Exit: &res.ExitCode}
			if err != nil {
				_, code := cpStatus(err)
				last = execFrame{Error: &cpFault{Code: code, Message: err.Error()}}
			}
			_ = fw.frame(last)
			b, _ := json.Marshal(last)
			return controlplane.Response{Status: http.StatusOK, Body: b}, nil
		})
	switch {
	case err != nil && !started:
		s.failCP(w, r, err)
	case replayed:
		w.Header().Set("Idempotent-Replayed", "true")
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(resp.Status)
		_, _ = w.Write(append(resp.Body, '\n'))
	case err != nil:
		// The stream has started and ended; the key could not be marked done.
		s.log.Warn("exec finished but its idempotency record was not completed", "path", r.URL.Path, "err", err)
	}
}
