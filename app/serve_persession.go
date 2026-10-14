package app

import (
	"errors"
	"fmt"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/builtin"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/sessionsandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// perSessionDriver is the driver sessions' sandboxes run under.
func perSessionDriver(cfg config.Config) string {
	if d := cfg.ControlPlane.Sessions.Driver; d != "" {
		return d
	}
	return builtin.Default
}

// checkPerSessionConfig refuses a per_session configuration that cannot
// work, before anything is built.
func checkPerSessionConfig(cfg config.Config) error {
	cp := cfg.ControlPlane
	if !cp.PerSession {
		return nil
	}
	if !cp.Enabled {
		return errors.New("control_plane.per_session needs control_plane.enabled")
	}
	if cp.SharedWorkspace {
		return errors.New("control_plane.per_session gives each session a folder of its own; it cannot be combined with control_plane.shared_workspace")
	}
	s := cp.Sessions
	switch s.Scope {
	case "", sessionsandbox.ScopeSession, sessionsandbox.ScopeUser:
	default:
		return fmt.Errorf("control_plane.sessions.scope %q: use %q or %q", s.Scope, sessionsandbox.ScopeSession, sessionsandbox.ScopeUser)
	}
	switch s.Retention {
	case "", sessionsandbox.RetainStop, sessionsandbox.RetainDelete, sessionsandbox.RetainKeep:
	default:
		return fmt.Errorf("control_plane.sessions.retention %q: use %q, %q or %q", s.Retention,
			sessionsandbox.RetainStop, sessionsandbox.RetainDelete, sessionsandbox.RetainKeep)
	}
	if s.WarmPool < 0 || s.WarmPool > controlplane.MaxPoolSize {
		return fmt.Errorf("control_plane.sessions.warm_pool %d: from 0 to %d", s.WarmPool, controlplane.MaxPoolSize)
	}
	if s.ReadySeconds < 0 {
		return fmt.Errorf("control_plane.sessions.ready_seconds %d is negative", s.ReadySeconds)
	}
	if t := s.Template; t != "" {
		// It names the warm pool in a label, so it must be a label value.
		if err := controlplane.ValidateLabels(map[string]string{"template": t}, false); err != nil {
			return fmt.Errorf("control_plane.sessions.template %q: letters, digits, '.', '_' and '-', at most 63", t)
		}
	}
	if !driver.ValidName(perSessionDriver(cfg)) {
		return fmt.Errorf("control_plane.sessions.driver %q is not a driver name", s.Driver)
	}
	// A process sandbox lives in the replica that made it; a session that
	// moves to another replica would find it gone.
	if cp.HA.Enabled && perSessionDriver(cfg) == builtin.Default {
		return errors.New("control_plane.per_session with control_plane.ha needs a session driver every replica reaches " +
			"(docker or podman on a shared engine, microvm, or an edition's cluster driver), not process")
	}
	return nil
}

// checkPerSessionDriver refuses a session driver that is not registered,
// cannot be reached, or cannot run a session's commands: every session
// would fail, so serve does not start.
func checkPerSessionDriver(cfg config.Config, drivers *driver.Registry, entries []builtin.Entry) error {
	if !cfg.ControlPlane.PerSession {
		return nil
	}
	name := perSessionDriver(cfg)
	d, err := drivers.Get(name)
	if err != nil {
		return fmt.Errorf("control_plane.sessions.driver %q is not registered here", name)
	}
	for _, e := range entries {
		if e.Name == name && e.Err != nil {
			return fmt.Errorf("control_plane.sessions.driver %s cannot be reached (%s): %w", name, e.Detail, e.Err)
		}
	}
	if !d.Info().Has(driver.CapCommand) {
		return fmt.Errorf("control_plane.sessions.driver %s cannot run a session's commands and terminals; use process, docker, podman or microvm", name)
	}
	return nil
}

// buildSessionSandboxes is the per-session manager over serve's control plane.
func buildSessionSandboxes(cfg config.Config, built builtControlPlane) (*sessionsandbox.Manager, error) {
	if err := checkPerSessionDriver(cfg, built.drivers, built.entries); err != nil {
		return nil, err
	}
	s := cfg.ControlPlane.Sessions
	tenant := cfg.Storage.Tenant
	if tenant == "" {
		tenant = "default"
	}
	return sessionsandbox.New(sessionsandbox.Config{
		Service: built.svc, Scope: s.Scope, Template: s.Template, Driver: perSessionDriver(cfg),
		Retention: s.Retention, Pool: s.WarmPool, Ready: time.Duration(s.ReadySeconds) * time.Second,
		Tenant: tenant, Workspace: built.workspace,
		Isolation: func(sb controlplane.Sandbox) tools.Isolation {
			iso := sandboxconfig.Isolation(cfg, sb.Tier)
			iso.Backend = sb.Driver + " driver, sandbox " + sb.ID
			return iso
		},
	})
}

// perSessionLabel says how sessions get their sandboxes, for the banner and doctor.
func perSessionLabel(cfg config.Config) string {
	cp := cfg.ControlPlane
	if !cp.PerSession {
		return "one shared by every session"
	}
	s := cp.Sessions
	scope := orDefault(s.Scope, sessionsandbox.ScopeSession)
	out := fmt.Sprintf("one per %s (driver %s, template %s, on stop: %s", scope, perSessionDriver(cfg),
		orDefault(s.Template, sessionsandbox.DefaultTemplate), orDefault(s.Retention, sessionsandbox.RetainStop))
	if s.WarmPool > 0 {
		out += fmt.Sprintf(", warm pool %d", s.WarmPool)
	}
	return out + ")"
}
