package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/zybuu-ai/abhed/controlplane"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/sessionsandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// bindSandbox gives a session being built its own sandbox, starting from
// the registry's bash for its secrets and settings.
func (s *Server) bindSandbox(sessionID string, spec StartSpec, registry *tools.Registry, rec *agent.Recorder) (sessionsandbox.Bound, error) {
	var shared tools.Bash
	if registry != nil {
		if t, ok := registry.Get("bash"); ok {
			shared, _ = t.(tools.Bash)
		}
	}
	b, err := s.opts.SessionSandboxes.Bind(context.Background(), sessionsandbox.Binding{
		SessionID: sessionID, User: spec.User, Tenant: storeTenant(s.opts.Config, spec.Tenant),
		Shared: shared, Recorder: rec,
	})
	if err != nil {
		return b, fmt.Errorf("the session's sandbox: %w", err)
	}
	return b, nil
}

// workspaceOf is the folder a live session works in.
func (s *Server) workspaceOf(live *liveSession) string {
	if live != nil && live.workspace != "" {
		return live.workspace
	}
	return s.opts.Workspace
}

// workspaceOfSess is the workspace a session's rows name: its sandbox's
// folder under per-session sandboxes, else the server's as configured.
func (s *Server) workspaceOfSess(sess *tools.Session) string {
	if s.opts.SessionSandboxes != nil {
		return sess.Root
	}
	return s.opts.Workspace
}

// errNoWorkspace is a session whose sandbox, and so whose files, are gone.
var errNoWorkspace = errors.New("the session has no workspace now")

// sessionWorkspace is the folder of session id for a caller already allowed
// to read it: the server's workspace, or under per-session sandboxes the
// folder of the session's own sandbox, never the server's in its place.
func (s *Server) sessionWorkspace(ctx context.Context, id string) (string, error) {
	if s.opts.SessionSandboxes == nil {
		return s.opts.Workspace, nil
	}
	tenant, user := TenantOf(ctx), UserOf(ctx)
	if live, ok := s.session(ctx, id, tenant, user); ok {
		return s.workspaceOf(live), nil
	}
	// The owner's user names a per-user sandbox; mayAccess has checked the caller is it.
	if g, ok := s.sessions.(sessionGetter); ok {
		if rec, err := g.GetSession(ctx, id); err == nil {
			user = rec.User
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	ws, err := s.opts.SessionSandboxes.Workspace(ctx, id, user, storeTenant(s.opts.Config, tenant))
	if errors.Is(err, sessionsandbox.ErrNoSandbox) {
		return "", errNoWorkspace
	}
	return ws, err
}

// sessionExists answers the sweep of sandboxes whose session is gone: only
// a session the store says it does not have is gone.
func (s *Server) sessionExists(ctx context.Context, id string) (bool, error) {
	if g, ok := s.sessions.(sessionGetter); ok {
		_, err := g.GetSession(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return err == nil, err
	}
	s.mu.RLock()
	_, live := s.running[id]
	s.mu.RUnlock()
	if live {
		return true, nil
	}
	events, err := s.store.Events(id)
	if err != nil {
		return false, err
	}
	return len(events) > 0, nil
}

// sessionSandboxScope asks the edition's SandboxCaller where a session's
// user acts, as for a create of their own, so the session's sandbox is made
// and counted there.
func (s *Server) sessionSandboxScope(ctx context.Context, tenant, user string) (map[string]string, error) {
	ctx = context.WithValue(context.WithValue(ctx, ctxUser, user), ctxTenant, tenant)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/sandboxes", http.NoBody)
	if err != nil {
		return nil, err
	}
	c, err := s.opts.SandboxCaller(r, controlplane.Caller{Principal: user})
	if err != nil {
		return nil, err
	}
	return c.Scope, nil
}
