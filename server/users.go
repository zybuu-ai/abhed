package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/auth"
)

// Local-account administration for the single-tenant server. The CLI twin is
// `abhed user`; this is the same set of facts reachable from the console.

type adminUser struct {
	Username string    `json:"username"`
	Email    string    `json:"email,omitempty"`
	Tenant   string    `json:"tenant"`
	Groups   []string  `json:"groups,omitempty"`
	Admin    bool      `json:"admin"`
	Created  time.Time `json:"created_at"`
}

// listUsers reports the accounts on this deployment.
func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	local := s.LocalAuth()
	if local == nil {
		WriteError(w, http.StatusNotImplemented,
			"this deployment does not hold its own accounts")
		return
	}
	users, err := local.ListUsers(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not list accounts")
		return
	}
	admin := s.adminGroup()
	out := make([]adminUser, 0, len(users))
	for _, u := range users {
		au := adminUser{
			Username: u.Username, Email: u.Email, Tenant: u.Tenant,
			Groups: u.Groups, Created: u.CreatedAt,
		}
		for _, g := range u.Groups {
			if g == admin {
				au.Admin = true
			}
		}
		out = append(out, au)
	}
	WriteJSON(w, http.StatusOK, out)
}

// setUserAdmin grants or revokes administrator rights.
func (s *Server) setUserAdmin(w http.ResponseWriter, r *http.Request) {
	local := s.LocalAuth()
	if local == nil {
		WriteError(w, http.StatusNotImplemented,
			"this deployment does not hold its own accounts")
		return
	}
	var req struct {
		Username string `json:"username"`
		Admin    bool   `json:"admin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Username == "" {
		WriteError(w, http.StatusBadRequest, "username is required")
		return
	}
	name := strings.ToLower(req.Username)
	admin := s.adminGroup()
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	if !req.Admin {
		// Compared on the subject, not UserOf, which is the email when the
		// account has one and so never matched a username.
		if id, ok := auth.FromContext(r.Context()); ok && strings.EqualFold(id.Subject, name) {
			WriteError(w, http.StatusConflict,
				"you cannot remove your own administrator rights")
			return
		}
		// Nor may the last administrator go, whoever removes them: with
		// nobody left, nothing on this deployment can grant it back.
		users, err := local.ListUsers(r.Context())
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "could not list accounts")
			return
		}
		target, others := false, 0
		for _, u := range users {
			if !slices.Contains(u.Groups, admin) {
				continue
			}
			if strings.EqualFold(u.Username, name) {
				target = true
			} else {
				others++
			}
		}
		if target && others == 0 {
			WriteError(w, http.StatusConflict,
				"this is the last administrator; make someone else an administrator first")
			return
		}
	}
	if err := local.SetGroups(r.Context(), name, admin, req.Admin); err != nil {
		WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	detail := map[string]any{"group": admin}
	action := "user.admin_granted"
	if !req.Admin {
		// Ended here, not left to the page: an API caller or a failed second
		// request would otherwise leave the rights on a live session.
		action = "user.admin_revoked"
		detail["sessions_ended"] = local.RevokeUser(name)
	}
	s.adminAudit(r, action, name, detail)
	w.WriteHeader(http.StatusNoContent)
}

// adminAudit logs an administrative change and hands it to Options.AdminAudit.
func (s *Server) adminAudit(r *http.Request, action, target string, detail map[string]any) {
	s.log.Info("admin action", "action", action, "target", target,
		"by", UserOf(r.Context()), "detail", detail)
	if s.opts.AdminAudit != nil {
		s.opts.AdminAudit(r.Context(), action, target, detail)
	}
}
