package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
)

// sessionIsolation is how serve keeps its accounts' sessions apart.
type sessionIsolation struct {
	// auto is set when per-session sandboxes were turned on because the
	// server has, or can gain, more than one account.
	auto bool
	// adminAPI is set when the control plane was off and turned on only for
	// those sandboxes: its own routes then answer administrators alone.
	adminAPI bool
	// sole is the one local account a shared server was started for; a
	// session for anyone else is refused.
	sole string
}

// serveSessionIsolation gives each session a sandbox of its own when the
// server has more than one account and the configuration says nothing,
// and otherwise names the one account a shared server serves.
func serveSessionIsolation(cfg config.Config, workspace string) (config.Config, sessionIsolation, error) {
	var iso sessionIsolation
	if cfg.ControlPlane.PerSession {
		return cfg, iso, nil
	}
	multi, sole, err := serveAccounts(cfg, workspace)
	if err != nil {
		return cfg, iso, err
	}
	if !multi {
		if !cfg.Server.SharedSessions {
			iso.sole = sole
		}
		return cfg, iso, nil
	}
	if cfg.Server.SharedSessions {
		return cfg, iso, nil
	}
	if cfg.ControlPlane.SharedWorkspace {
		return cfg, iso, errors.New("this server has more than one account, so each session gets a sandbox of its own, " +
			"which control_plane.shared_workspace forbids: set server.shared_sessions to keep every account in one workspace, " +
			"or unset control_plane.shared_workspace")
	}
	iso.adminAPI = !cfg.ControlPlane.Enabled
	cfg.ControlPlane.Enabled = true
	cfg.ControlPlane.PerSession = true
	iso.auto = true
	return cfg, iso, nil
}

// serveAccounts reports whether serve has, or can gain while it runs, more
// than one account, and the owner of the one local account when it cannot.
// With authentication off there is one person. A provider other than local
// accounts, or open sign-up, can bring anyone; so can local accounts with
// none yet.
func serveAccounts(cfg config.Config, workspace string) (bool, string, error) {
	modes := authModes(cfg)
	if len(modes) == 0 {
		return false, "", nil
	}
	for _, m := range modes {
		if m != "local" {
			return true, "", nil
		}
	}
	if cfg.Auth.AllowSignup {
		return true, "", nil
	}
	users, err := userStore(cfg, workspace)
	if err != nil {
		return false, "", err
	}
	if c, ok := users.(interface{ Close() }); ok {
		defer c.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list, err := users.List(ctx)
	if err != nil {
		return false, "", fmt.Errorf("count the accounts, to decide whether sessions share a workspace: %w", err)
	}
	if len(list) != 1 {
		return true, "", nil
	}
	return false, auth.LocalOwner(list[0].Username), nil
}
