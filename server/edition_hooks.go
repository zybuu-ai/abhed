package server

import (
	"context"
	"net/http"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// DenyPending denies the ask session id waits on, when it is request
// requestID and the session runs here for owner in tenant. It reports
// whether the denial decided the request: false when another answer got
// there first, the request is not the one waiting, or the session is not
// here. The store's row, when there is one, is the arbiter, as it is for
// every answer. The denial is recorded as the system's, with reason.
func (s *Server) DenyPending(ctx context.Context, tenant, owner, id, requestID, reason string) bool {
	if owner == "" || requestID == "" || reason == "" {
		return false
	}
	s.mu.RLock()
	live, ok := s.running[id]
	mine := ok && live.Tenant == tenant && live.User == owner
	s.mu.RUnlock()
	if !mine {
		return false
	}
	live.mu.Lock()
	p := live.pending
	waiting := p != nil && p.RequestID == requestID && p.state == askWaiting
	live.mu.Unlock()
	if !waiting {
		return false
	}
	// The row the request is recorded on must be settled first.
	select {
	case <-p.ready:
	case <-p.final:
		return false
	case <-ctx.Done():
		return false
	case <-time.After(approvalReadyWait.Get()):
		return false
	}
	live.mu.Lock()
	durableID := p.DurableID
	live.mu.Unlock()
	if d := s.approvalStore(); d != nil && durableID != "" {
		answered, err := d.AnswerApproval(ctx, durableID, false, "", "system")
		if err != nil || !answered {
			return false
		}
	}
	live.mu.Lock()
	held := live.move(p, askAnswered, false, "")
	if held {
		p.system = reason
	}
	live.mu.Unlock()
	return held || durableID != ""
}

// UpstreamTransport sends an edition's own outbound requests, such as a
// notice to an operator's webhook, through the upstream proxy the operator
// configured (network.upstream_proxy), read at each request; with none,
// directly.
func UpstreamTransport() http.RoundTripper { return egress.UpstreamRoundTripper{} }
