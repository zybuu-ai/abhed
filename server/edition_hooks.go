package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/auth"
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
		answered, err := d.AnswerApproval(ctx, durableID, false, "", systemApprover)
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

// systemApprover is who a row DenyPending answered names. No account can
// hold it: an owner under the nobody prefix owns nothing and answers nothing.
const systemApprover = auth.NobodyPrefix + "system-timeout"

// UpstreamTransport sends an edition's own outbound requests, such as a
// notice to an operator's webhook, through the upstream proxy the operator
// configured (network.upstream_proxy), read at each request; with none,
// directly.
func UpstreamTransport() http.RoundTripper { return egress.UpstreamRoundTripper{} }

// ErrNoOwnWorkspace refuses a StartSpec.Prepare for a session with no folder
// of its own: one that shares the server's workspace, or its user's.
var ErrNoOwnWorkspace = errors.New("this server gives sessions no folder of their own " +
	"(per-session sandboxes with control_plane.sessions.scope \"session\"), so nothing can be put in one")

// errPrepare is a StartSpec.Prepare that failed; its cause goes to the log.
var errPrepare = errors.New("the session's folder could not be prepared")

// prepareWait bounds a StartSpec.Prepare.
const prepareWait = 5 * time.Minute

// prepareWorkspace runs spec.Prepare on root, the folder of the session's
// own sandbox, and removes that sandbox when it fails, so a half-filled
// folder is not left behind.
func (s *Server) prepareWorkspace(sessionID string, spec StartSpec, bound bool, root string) error {
	if !bound || !s.opts.SessionSandboxes.SessionScoped() {
		return ErrNoOwnWorkspace
	}
	ctx, cancel := context.WithTimeout(context.Background(), prepareWait)
	defer cancel()
	if err := spec.Prepare(ctx, root); err != nil {
		s.log.Error("a session's folder was not prepared", "session", sessionID, "err", err)
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if derr := s.opts.SessionSandboxes.SessionDeleted(dctx, sessionID, spec.User, storeTenant(s.opts.Config, spec.Tenant)); derr != nil {
			s.log.Warn("removing the sandbox of a session not prepared", "session", sessionID, "err", derr)
		}
		return errPrepare
	}
	return nil
}

// SessionPatch is what session id, run here for owner in tenant, changed in
// its folder, as one unified diff with each file's diff as GET .../changes
// gives it, cut at max bytes (truncated then says so). available is false
// when this process does not hold the session's originals. Files the
// server's read rules keep from a viewer are named, not shown.
func (s *Server) SessionPatch(tenant, owner, id string, max int) (patch string, available, truncated bool, err error) {
	s.mu.RLock()
	live, ok := s.running[id]
	mine := ok && owner != "" && live.Tenant == tenant && live.User == owner
	s.mu.RUnlock()
	if !mine || live.undo == nil {
		return "", false, false, nil
	}
	v, err := s.openView(s.workspaceOf(live))
	if err != nil {
		return "", false, false, err
	}
	defer v.Close()
	var b strings.Builder
	add := func(text string) bool {
		if b.Len()+len(text) > max {
			b.WriteString(text[:max-b.Len()])
			return false
		}
		b.WriteString(text)
		return true
	}
	for _, f := range s.changesOf(live, v).Files {
		text := f.Diff
		if text == "" {
			text = fmt.Sprintf("# %s: %s, no diff (%s)\n", f.Path, f.Status, orDefaultStr(f.Note, "unchanged text"))
		} else if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if !add(text) {
			return b.String(), true, true, nil
		}
	}
	return b.String(), true, false, nil
}

// DeleteSessionSandbox removes the sandbox, and with it the folder, of
// session id, when the session runs here for owner in tenant and the
// sandbox is its own. The session can do no more work after it.
func (s *Server) DeleteSessionSandbox(ctx context.Context, tenant, owner, id string) error {
	s.mu.RLock()
	live, ok := s.running[id]
	mine := ok && owner != "" && live.Tenant == tenant && live.User == owner
	s.mu.RUnlock()
	if !mine || s.opts.SessionSandboxes == nil || !s.opts.SessionSandboxes.SessionScoped() {
		return nil
	}
	return s.opts.SessionSandboxes.SessionDeleted(ctx, id, owner, storeTenant(s.opts.Config, tenant))
}
