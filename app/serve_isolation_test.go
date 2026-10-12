package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
)

// A server with more than one account, or one that can gain more, gives each
// session a sandbox of its own unless server.shared_sessions says to share;
// one with a single local account names that account; one with sign-in off
// is left as it was.
func TestServeSessionIsolation(t *testing.T) {
	ws := t.TempDir()
	withUsers := func(names ...string) config.Config {
		cfg := config.Default()
		cfg.Auth.Mode = "local"
		cfg.Auth.UsersFile = filepath.Join(t.TempDir(), "users.json")
		st, err := auth.NewFileUserStore(cfg.Auth.UsersFile)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if err := auth.NewLocalAuth(st, 0, false).CreateUser(context.Background(), auth.User{Username: n}, "correct-horse-1"); err != nil {
				t.Fatal(err)
			}
		}
		return cfg
	}
	mode := func(m string) config.Config { c := config.Default(); c.Auth.Mode = m; return c }
	signup := withUsers("ann")
	signup.Auth.AllowSignup = true
	shared := withUsers("ann", "bob")
	shared.Server.SharedSessions = true
	sharedOne := withUsers("ann")
	sharedOne.Server.SharedSessions = true
	for _, c := range []struct {
		name string
		cfg  config.Config
		auto bool
		sole string
	}{
		{"sign-in off", config.Default(), false, ""},
		{"one local account", withUsers("ann"), false, "local:ann"},
		{"one account, sharing on", sharedOne, false, ""},
		{"two local accounts", withUsers("ann", "bob"), true, ""},
		{"no account yet", withUsers(), true, ""},
		{"open sign-up", signup, true, ""},
		{"single sign-on", mode("oidc"), true, ""},
		{"a trusted proxy", mode("proxy"), true, ""},
		{"two accounts, sharing on", shared, false, ""},
	} {
		got, iso, err := serveSessionIsolation(c.cfg, ws)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if iso.auto != c.auto || iso.sole != c.sole {
			t.Fatalf("%s: auto %v sole %q, want %v %q", c.name, iso.auto, iso.sole, c.auto, c.sole)
		}
		if c.auto && (!got.ControlPlane.Enabled || !got.ControlPlane.PerSession) {
			t.Fatalf("%s: per-session sandboxes were not turned on: %+v", c.name, got.ControlPlane)
		}
		if !c.auto && got.ControlPlane.PerSession != c.cfg.ControlPlane.PerSession {
			t.Fatalf("%s: per_session changed", c.name)
		}
	}
	// Several accounts and control_plane.shared_workspace contradict each other.
	bad := withUsers("ann", "bob")
	bad.ControlPlane.SharedWorkspace = true
	if _, _, err := serveSessionIsolation(bad, ws); err == nil {
		t.Fatal("shared_workspace with several accounts was accepted")
	}
	bad.Server.SharedSessions = true
	if _, _, err := serveSessionIsolation(bad, ws); err != nil {
		t.Fatalf("with shared_sessions set: %v", err)
	}
}

// Under HA, a server with several accounts gets per-session sandboxes by
// itself, and the process driver is refused for them as for a set per_session.
func TestServeSessionIsolationUnderHANeedsASharedDriver(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "oidc"
	cfg.ControlPlane.HA.Enabled = true
	got, iso, err := serveSessionIsolation(cfg, t.TempDir())
	if err != nil || !iso.auto {
		t.Fatalf("auto %v: %v", iso.auto, err)
	}
	if err := checkPerSessionConfig(got); err == nil {
		t.Fatal("several accounts under HA on the process driver were accepted")
	}
	got.ControlPlane.Sessions.Driver = "podman"
	if err := checkPerSessionConfig(got); err != nil {
		t.Fatalf("with a shared-engine driver: %v", err)
	}
	cfg.Server.SharedSessions = true
	if got, _, _ := serveSessionIsolation(cfg, t.TempDir()); got.ControlPlane.PerSession {
		t.Fatal("shared_sessions still turned per_session on")
	}
}
