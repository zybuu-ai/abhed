package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// ApprovalGate lets an edition with approvers decide who may answer a
// session's pending approval and when an answer settles it, on the same
// approvals the console and POST /v1/sessions/{id}/approve answer.
type ApprovalGate interface {
	// MayAnswer reports whether user, who does not own the session, may
	// answer its approvals. It must answer false when it cannot tell.
	MayAnswer(ctx context.Context, tenant, owner, user string) bool
	// Answer is told of one answer before it is recorded on the request. It
	// returns true when this answer settles the request, which then reaches
	// the turn as any answer does, and false when it is held as one of
	// several the request needs, which keeps waiting. An error refuses the
	// answer: ErrAnswerRefused is 403, anything else 503.
	Answer(ctx context.Context, a ApprovalAnswer) (settles bool, err error)
}

// ErrAnswerRefused is an answer the approval gate does not accept, such as
// a person approving their own request.
var ErrAnswerRefused = errors.New("this answer is refused")

// ApprovalAnswer is one person's answer to a pending approval.
type ApprovalAnswer struct {
	Tenant    string
	SessionID string
	// Owner is who the session runs as: the person whose request this is.
	Owner string
	// Request identifies the request: its stored approval row when the store
	// keeps one, else its action.requested event id.
	Request string
	Tool    string
	// Args are the call's arguments as the record keeps them, redacted.
	Args     json.RawMessage
	Reason   string
	Since    time.Time
	By       string
	Approved bool
	Scope    string
}

// PendingAsk is an approval waiting in a session this process runs.
type PendingAsk struct {
	SessionID string          `json:"session_id"`
	Tenant    string          `json:"tenant"`
	Owner     string          `json:"owner"`
	Request   string          `json:"request"`
	RequestID string          `json:"request_id,omitempty"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Subagent  string          `json:"subagent,omitempty"`
	Since     time.Time       `json:"since"`
}

// PendingAsks lists the approvals waiting in sessions this process runs
// whose tenant and owner include accepts, oldest first. A session another
// node runs is listed by that node.
func (s *Server) PendingAsks(include func(tenant, owner string) bool) []PendingAsk {
	s.mu.RLock()
	lives := make([]*liveSession, 0, len(s.running))
	for _, l := range s.running {
		if include(l.Tenant, l.User) {
			lives = append(lives, l)
		}
	}
	s.mu.RUnlock()
	var out []PendingAsk
	for _, l := range lives {
		l.mu.Lock()
		p := l.pending
		var ask PendingAsk
		if p != nil && p.state == askWaiting {
			ask = PendingAsk{SessionID: l.ID, Tenant: l.Tenant, Owner: l.User, Request: requestKey(p),
				RequestID: p.RequestID, Tool: p.Tool, Reason: p.Reason, Subagent: p.Subagent, Since: p.Since}
			ask.Args = redactedArgs(l, p.Args)
		}
		l.mu.Unlock()
		if ask.SessionID != "" {
			out = append(out, ask)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// requestKey is what identifies a request to the gate, whichever node an
// answer reaches: its stored row when there is one. Called with l.mu held.
func requestKey(p *pendingApproval) string {
	if p.DurableID != "" {
		return p.DurableID
	}
	return p.RequestID
}

// redactedArgs are a call's arguments as the session's record would keep
// them; without a redactor, or when redaction breaks them, they are withheld.
func redactedArgs(l *liveSession, args json.RawMessage) json.RawMessage {
	if len(args) == 0 {
		return nil
	}
	var red agent.Redactor
	if l.Loop != nil && l.Loop.Recorder != nil {
		red = l.Loop.Recorder.Redact
	}
	if red == nil || (reflect.ValueOf(red).Kind() == reflect.Pointer && reflect.ValueOf(red).IsNil()) {
		return json.RawMessage(`{"withheld":true}`)
	}
	out := red.Redact(append([]byte(nil), args...))
	if !json.Valid(out) {
		return json.RawMessage(`{"withheld":true}`)
	}
	return out
}

// approverSession is a live session the caller does not own but whose
// approvals the gate lets them answer.
func (s *Server) approverSession(ctx context.Context, id, tenant, user string) (*liveSession, bool) {
	g := s.opts.ApprovalGate
	if g == nil || user == "" || user == auth.Anonymous || auth.OwnsNothing(user) {
		return nil, false
	}
	s.mu.RLock()
	live, found := s.running[id]
	s.mu.RUnlock()
	if !found || !s.stillOwned(ctx, live, id) {
		return nil, false
	}
	s.mu.RLock()
	owner, inTenant := live.User, live.Tenant == tenant
	s.mu.RUnlock()
	if !inTenant || owner == user {
		return nil, false
	}
	return live, g.MayAnswer(ctx, tenant, owner, user)
}

// gateAnswer asks the gate about an answer, when there is one. It reports
// whether the answer goes on to the request; when it does not, it has
// answered the HTTP request.
func (s *Server) gateAnswer(w http.ResponseWriter, r *http.Request, a ApprovalAnswer) bool {
	g := s.opts.ApprovalGate
	if g == nil {
		return true
	}
	settles, err := g.Answer(r.Context(), a)
	switch {
	case errors.Is(err, ErrAnswerRefused):
		WriteError(w, http.StatusForbidden, err.Error())
		return false
	case err != nil:
		s.log.Error("approval gate failed", "session", a.SessionID, "err", err)
		w.Header().Set("Retry-After", "5")
		WriteError(w, http.StatusServiceUnavailable, "the answer could not be recorded; retry")
		return false
	case !settles:
		WriteJSON(w, http.StatusAccepted, map[string]bool{"recorded": true, "settled": false})
		return false
	}
	return true
}

// storedAnswerer reads a stored session's tenant and owner, and whether the
// caller may answer its approvals: as its owner, or as the gate allows.
func (s *Server) storedAnswerer(r *http.Request, id string) (tenant, owner string, ok bool) {
	g, isGetter := s.sessions.(sessionGetter)
	if !isGetter {
		return TenantOf(r.Context()), UserOf(r.Context()), s.mayAccess(r, id)
	}
	rec, err := g.GetSession(r.Context(), id)
	if err != nil {
		return "", "", false
	}
	tenant, user := TenantOf(r.Context()), UserOf(r.Context())
	if ownsSession(rec.Tenant, rec.User, tenant, user) {
		return rec.Tenant, rec.User, true
	}
	gate := s.opts.ApprovalGate
	if gate == nil || rec.Tenant != tenant || user == "" || user == auth.Anonymous || auth.OwnsNothing(user) {
		return "", "", false
	}
	return rec.Tenant, rec.User, gate.MayAnswer(r.Context(), rec.Tenant, rec.User, user)
}

// withheldArgs stands in for a stored row's arguments, which are not
// redacted here: the gate is told the tool and reason, never the values.
func withheldArgs(args []byte) json.RawMessage {
	if len(args) == 0 {
		return nil
	}
	return json.RawMessage(`{"withheld":true}`)
}
