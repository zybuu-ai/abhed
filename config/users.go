package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WorkspaceUsersFile is where a workspace keeps local accounts when
// auth.users_file is not set.
func WorkspaceUsersFile(workspace string) string {
	return filepath.Join(workspace, ".abhed", "users.json")
}

// UsersSource is where local accounts are kept, and whether they may be read.
type UsersSource struct {
	Path string
	// Ignored is set for a file inside a workspace that is not trusted: a
	// repository could otherwise bring its own accounts, and passwords, with it.
	Ignored bool
}

// UsersFile resolves auth.users_file for workspace: relative to it, and
// <workspace>/.abhed/users.json when not set. A file inside the workspace is
// read only when the workspace is trusted, unless the managed configuration
// names it.
func (c Config) UsersFile(workspace string) UsersSource {
	p := c.Auth.UsersFile
	switch {
	case p == "":
		p = WorkspaceUsersFile(workspace)
	case !filepath.IsAbs(p):
		// Relative to the workspace, so serve, user and migrate find one file
		// whichever directory each was started in.
		p = filepath.Join(workspace, p)
	}
	src := UsersSource{Path: p}
	if c.Auth.UsersFile != "" && c.ManagedSets("auth.users_file") {
		return src
	}
	ws := canonical(workspace)
	if !inside(workspace, p) {
		return src
	}
	trusted := c.Workspace.UsersTrusted
	if c.Workspace.Workspace == "" || canonical(c.Workspace.Workspace) != ws {
		// Not loaded for this workspace: decide as a load would, from the store.
		trusted, _ = decideUsers(WorkspaceTrust{Workspace: ws}, LoadOptions{})
	}
	src.Ignored = !trusted
	return src
}

// inside reports whether path lies in workspace as written (under the
// workspace as given or its canonical form) or once fully resolved: a
// <workspace>/.abhed that is a link elsewhere still sits in the workspace.
func inside(workspace, path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	under := func(dir, p string) bool {
		rel, err := filepath.Rel(dir, p)
		return err == nil && filepath.IsLocal(rel)
	}
	ws := canonical(workspace)
	if given, err := filepath.Abs(workspace); err == nil && under(given, abs) {
		return true
	}
	return under(ws, abs) || under(ws, resolveExisting(abs))
}

// resolveExisting resolves the links in the longest part of path that
// exists, and keeps the rest as written.
func resolveExisting(path string) string {
	rest := ""
	for p := path; ; p = filepath.Dir(p) {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		if filepath.Dir(p) == p {
			return path
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}

// UsersIgnoredWarning names an ignored accounts file, or where one would be,
// and how to trust its workspace; "" when nothing is ignored.
func (c Config) UsersIgnoredWarning(workspace string) string {
	src := c.UsersFile(workspace)
	if !src.Ignored {
		return ""
	}
	how := "Trust the workspace with `abhed trust grant` (or run with -trust-workspace), " +
		"or set auth.users_file outside the workspace in ~/.abhed/config.json or the managed configuration"
	if !exists(src.Path) {
		return fmt.Sprintf("accounts would be kept in %s, inside the workspace %s, which is not trusted, "+
			"so Abhed neither reads nor writes accounts there. %s", Printable(src.Path), Printable(canonical(workspace)), how)
	}
	return fmt.Sprintf("ignoring the accounts in %s: the workspace %s is not trusted, so no account from it can sign in. "+
		"%s, moving the file there", Printable(src.Path), Printable(canonical(workspace)), how)
}

// usersAttempt is the record of an ignored accounts file: there is one,
// local accounts are in use, and the workspace is not trusted.
func (c Config) usersAttempt() (Attempt, bool) {
	ws := c.Workspace.Workspace
	if ws == "" || c.Auth.Mode != "local" {
		return Attempt{}, false
	}
	src := c.UsersFile(ws)
	if !src.Ignored {
		return Attempt{}, false
	}
	if _, err := os.Lstat(src.Path); err != nil {
		return Attempt{}, false
	}
	return Attempt{Layer: LayerWorkspace, Source: src.Path, Key: "auth.users_file", Value: src.Path,
		Decision: "ignored_untrusted",
		Reason:   "the workspace is not trusted, so the accounts file inside it was not read"}, true
}

// decideUsers decides whether accounts files inside the workspace are read:
// the home directory's are the user's own, a trusted configuration file
// trusts them, and otherwise -trust-workspace, TrustEnv or a stored grant.
// The decision is not keyed by content: accounts change as they are managed.
func decideUsers(st WorkspaceTrust, o LoadOptions) (bool, string) {
	if isHome(st.Workspace) {
		return true, "home"
	}
	if st.Trusted {
		return true, st.Reason
	}
	switch o.Trust {
	case TrustRefused:
		return false, "refused"
	case TrustGranted:
		return true, "flag"
	}
	if v := os.Getenv(TrustEnv); v == "1" || strings.EqualFold(v, "true") {
		return true, "env"
	}
	e, ok, err := lookupTrust(st.Workspace)
	switch {
	case err != nil || !ok || e.Users == "":
		return false, "new"
	case e.Users == decisionTrusted:
		return true, "stored"
	}
	return false, "declined"
}

// noteUsers fills in the workspace's accounts file and the decision on it.
func noteUsers(st *WorkspaceTrust, workspace string, o LoadOptions) {
	if p := WorkspaceUsersFile(workspace); exists(p) {
		st.UsersFile = p
	}
	st.UsersTrusted, st.UsersReason = decideUsers(*st, o)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// GrantUsers records that the person trusts the accounts files inside the
// workspace. It keeps every other decision about the workspace.
func GrantUsers(workspace string) error {
	key := canonical(workspace)
	return updateTrust(func(m map[string]TrustRecord) {
		rec := m[key]
		rec.Users = decisionTrusted
		if rec.At.IsZero() {
			rec.At = time.Now().UTC()
		}
		m[key] = rec
	})
}
