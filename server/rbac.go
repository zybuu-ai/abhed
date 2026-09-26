package server

import (
	"net/http"

	"github.com/zybuu-ai/abhed/auth"
)

// Administration: the routes that change what the deployment is, rather than
// what one session does.
//
// Every local account was equal until now. That was survivable while the only
// thing a user could do was run their own sessions, and stops being survivable
// the moment the console can add a skill or an MCP server — a skill is
// *instructions*, and an MCP server is a *tool surface*, so letting any
// signed-in user add either means letting them rewrite what the agent does.
//
// The group plumbing already existed end to end: auth.User.Groups is persisted
// by both stores and copied into the session Identity, and auth.RequireGroup
// checks it. What was missing was anything that set the group, and anywhere
// that checked it per-route.

// DefaultAdminGroup is used when the operator names none.
const DefaultAdminGroup = "abhed-admin"

func (s *Server) adminGroup() string {
	if g := s.opts.Config.Auth.AdminGroup; g != "" {
		return g
	}
	return DefaultAdminGroup
}

// Admin wraps a handler so only members of the admin group reach it.
//
// Registered per-route rather than around the mux, as Config.Auth.RequireGroup
// is: that one says who may use the deployment at all, this who may change it.
//
// Ordering works out because auth.Middleware.Wrap sits outside the mux: by the
// time a route is dispatched, the identity is already in the context. Exported
// so a mounted route is gated by the same group as a built-in one, rather than
// by a second definition of "administrator" that could drift from this one.
func (s *Server) Admin(h http.HandlerFunc) http.Handler {
	return auth.RequireGroup(s.adminGroup(), h)
}

// IsAdmin reports whether the caller is an administrator, for handlers that
// change their answer rather than refusing outright.
func (s *Server) IsAdmin(r *http.Request) bool {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		return false
	}
	for _, g := range id.Groups {
		if g == s.adminGroup() {
			return true
		}
	}
	return false
}
