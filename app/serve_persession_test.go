package app

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/sessionsandbox"
)

func TestPerSessionConfigIsChecked(t *testing.T) {
	ok := config.Default()
	ok.ControlPlane.Enabled, ok.ControlPlane.PerSession = true, true
	if err := checkPerSessionConfig(ok); err != nil {
		t.Fatal(err)
	}
	if err := checkPerSessionConfig(config.Default()); err != nil {
		t.Fatalf("off: %v", err)
	}
	for name, edit := range map[string]func(*config.ControlPlaneConfig){
		"control_plane.enabled":  func(c *config.ControlPlaneConfig) { c.Enabled = false },
		"shared_workspace":       func(c *config.ControlPlaneConfig) { c.SharedWorkspace = true },
		"sessions.scope":         func(c *config.ControlPlaneConfig) { c.Sessions.Scope = "tenant" },
		"sessions.retention":     func(c *config.ControlPlaneConfig) { c.Sessions.Retention = "archive" },
		"sessions.warm_pool":     func(c *config.ControlPlaneConfig) { c.Sessions.WarmPool = 17 },
		"sessions.template":      func(c *config.ControlPlaneConfig) { c.Sessions.Template = "has space" },
		"sessions.driver":        func(c *config.ControlPlaneConfig) { c.Sessions.Driver = "Bad Name" },
		"sessions.ready_seconds": func(c *config.ControlPlaneConfig) { c.Sessions.ReadySeconds = -1 },
	} {
		cfg := ok
		edit(&cfg.ControlPlane)
		err := checkPerSessionConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), strings.TrimSpace(name)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Shared and per-session at once is refused where the folders are made, too.
	cp := ok.ControlPlane
	cp.SharedWorkspace = true
	if _, _, err := sandboxWorkspaces(cp, t.TempDir()); err == nil {
		t.Fatal("per-session with a shared workspace was accepted")
	}
}

func TestServeCheckPerSession(t *testing.T) {
	ws := checkEnv(t, `{"control_plane":{"enabled":true,"per_session":true,"sessions":{"warm_pool":2}}}`, "")
	var out bytes.Buffer
	if code := newApp().serveCheck(&out, ws, "127.0.0.1:0"); code != 0 ||
		!strings.Contains(out.String(), "per-session sandboxes") || !strings.Contains(out.String(), "one per session (driver process, template session, on stop: stop, warm pool 2)") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	// A driver that cannot run a session's commands, or is not there, fails it.
	for _, drv := range []string{"microvm", "docker"} {
		ws = checkEnv(t, `{"control_plane":{"enabled":true,"per_session":true,"sessions":{"driver":"`+drv+`"}}}`, "")
		out.Reset()
		if code := newApp().serveCheck(&out, ws, "127.0.0.1:0"); code != 1 || !strings.Contains(out.String(), "FAIL per-session sandboxes") ||
			!strings.Contains(out.String(), "not registered") {
			t.Fatalf("driver %s: exit %d:\n%s", drv, code, out.String())
		}
	}
	// An engine that does not answer fails its line and the session driver's.
	engine := filepath.Join(t.TempDir(), "podman")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ws = checkEnv(t, `{"control_plane":{"enabled":true,"per_session":true,"sessions":{"driver":"podman"},"podman":{"enabled":true}}}`,
		`{"control_plane":{"podman":{"binary":"`+engine+`"}}}`)
	out.Reset()
	if code := newApp().serveCheck(&out, ws, "127.0.0.1:0"); code != 1 || !strings.Contains(out.String(), "FAIL driver podman") ||
		!strings.Contains(out.String(), "cannot be reached") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
}

// Two sessions on serve's own control plane and process driver, on this
// host's real sandbox: each one's commands work in its own folder and can
// neither read nor write the other's.
func TestPerSessionOnTheRealProcessDriver(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the host's sandbox")
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		if _, err := exec.LookPath("bwrap"); err != nil {
			t.Skip("no process tier on this host")
		}
	}
	ws := checkEnv(t, `{"control_plane":{"enabled":true,"per_session":true}}`, "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg, err := newApp().loadConfig(ws)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Sandbox.ProtectGit = true
	ctx := context.Background()
	built, err := buildControlPlane(ctx, cfg, ws, agent.NewMemStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer built.close()
	mgr, err := buildSessionSandboxes(cfg, built)
	if err != nil {
		t.Fatal(err)
	}
	bind := func(session, user string) sessionsandbox.Bound {
		b, err := mgr.Bind(ctx, sessionsandbox.Binding{SessionID: session, User: user, Tenant: "default"})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	a, b := bind("s1", "alice"), bind("s2", "bob")
	defer func() { _ = mgr.Close(context.Background()) }()
	if a.Workspace == b.Workspace || a.Sandbox.Tier == "none" || a.Bash.Isolation.Tier != a.Sandbox.Tier {
		t.Fatalf("alice %+v at %s, bob at %s", a.Sandbox, a.Workspace, b.Workspace)
	}
	run := func(bound sessionsandbox.Bound, line string) (string, bool) {
		cmd := bound.Bash.Sandbox(ctx, bound.Workspace, line)
		if cmd.Err != nil {
			t.Fatal(cmd.Err)
		}
		out, err := cmd.CombinedOutput()
		return string(out), err == nil
	}
	if out, ok := run(b, "echo bob > secret.txt && pwd"); !ok || !strings.Contains(out, b.Workspace) {
		t.Fatalf("bob's own folder: %q", out)
	}
	if out, ok := run(a, "cat "+quote(filepath.Join(b.Workspace, "secret.txt"))); ok || strings.Contains(out, "bob") {
		t.Fatalf("alice read bob's file: %q", out)
	}
	if out, ok := run(a, "echo x > "+quote(filepath.Join(b.Workspace, "planted.txt"))); ok {
		t.Fatalf("alice wrote into bob's folder (tier %s, %s): %q", a.Sandbox.Tier, a.Workspace, out)
	}
	if _, err := os.Stat(filepath.Join(b.Workspace, "planted.txt")); err == nil {
		t.Fatal("a file was planted in bob's folder")
	}
	// The terminal's shell is in the sandbox too.
	sh := a.Bash.Shell(ctx, a.Workspace)
	if sh.Err != nil {
		t.Fatal(sh.Err)
	}
	sh.Stdin = strings.NewReader("cat " + quote(filepath.Join(b.Workspace, "secret.txt")) + "; exit 0\n")
	if out, _ := sh.CombinedOutput(); strings.Contains(string(out), "bob\n") {
		t.Fatalf("alice's terminal read bob's file: %q", out)
	}
	// What ran is in the control plane's record, as alice.
	evs, _ := built.svc.Store().(controlplane.AuditReader).AuditFor(ctx, a.Sandbox.ID)
	execs := 0
	for _, ev := range evs {
		if ev.Action == "exec" && ev.Principal == "alice" && ev.Detail["via"] == "session" {
			execs++
		}
	}
	if execs < 3 {
		t.Fatalf("%d of alice's commands recorded: %+v", execs, evs)
	}
}

// quote is a path as one shell word.
func quote(p string) string { return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'" }
