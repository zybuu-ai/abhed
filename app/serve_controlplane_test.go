package app

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/builtin"
	"github.com/zybuu-ai/abhed/internal/managed"
)

// checkEnv isolates the configuration: userCfg is the person's own file and
// managedCfg, when set, the managed one.
func checkEnv(t *testing.T, userCfg, managedCfg string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	t.Setenv(config.TrustEnv, "")
	old := managed.ConfigFile
	managed.ConfigFile = filepath.Join(t.TempDir(), "absent.json")
	if managedCfg != "" {
		if err := os.WriteFile(managed.ConfigFile, []byte(managedCfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { managed.ConfigFile = old })
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(userCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return t.TempDir()
}

func TestServeCheckPasses(t *testing.T) {
	ws := checkEnv(t, `{"control_plane":{"enabled":true,"exec_timeout_seconds":120}}`,
		`{"control_plane":{"restart_policy":"on-failure"}}`)
	var out bytes.Buffer
	code := newApp().serveCheck(&out, ws, "127.0.0.1:0")
	if code != 0 || !strings.Contains(out.String(), "Ready to serve.") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	for _, want := range []string{
		"control plane", "on, store memory",
		"exec.timeout_seconds", "120 (configured)",
		"restart.policy", "on-failure (managed, ceiling)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestServeCheckFails(t *testing.T) {
	ws := checkEnv(t, `{"control_plane":{"enabled":true,"restart_policy":"sometimes"}}`, "")
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	var out bytes.Buffer
	if code := newApp().serveCheck(&out, ws, held.Addr().String()); code != 1 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	for _, want := range []string{"FAIL control plane", "restart.policy", "FAIL listen", "Not ready: 2 check(s) failed."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	// Off, the control plane is not checked and says how to turn it on.
	ws = checkEnv(t, `{}`, "")
	out.Reset()
	if code := newApp().serveCheck(&out, ws, "127.0.0.1:0"); code != 0 || !strings.Contains(out.String(), "off (control_plane.enabled)") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
}

func TestSandboxWorkspaces(t *testing.T) {
	ws := t.TempDir()
	// Shared only when asked for, and never together with a root.
	get, release, err := sandboxWorkspaces(config.ControlPlaneConfig{SharedWorkspace: true}, ws)
	if err != nil || release != nil {
		t.Fatal(err)
	}
	if d, _ := get("sbx_0123456789abcdef"); d != ws {
		t.Fatalf("shared workspace %q", d)
	}
	if _, _, err := sandboxWorkspaces(config.ControlPlaneConfig{SharedWorkspace: true, WorkspaceRoot: t.TempDir()}, ws); err == nil {
		t.Fatal("shared_workspace with a root was accepted")
	}
	for _, bad := range []string{"relative/root", ws, filepath.Join(ws, "sandboxes"), filepath.Dir(ws),
		filepath.Join(t.TempDir(), ".abhed", "sandboxes"), filepath.Join(t.TempDir(), ".ABHED")} {
		if _, _, err := sandboxWorkspaces(config.ControlPlaneConfig{WorkspaceRoot: bad}, ws); err == nil {
			t.Errorf("root %q was accepted", bad)
		}
	}
	root := filepath.Join(t.TempDir(), "sandboxes")
	get, release, err = sandboxWorkspaces(config.ControlPlaneConfig{WorkspaceRoot: root}, ws)
	if err != nil {
		t.Fatal(err)
	}
	d, err := get("sbx_0123456789abcdef")
	if err != nil || d != filepath.Join(root, "sbx_0123456789abcdef") {
		t.Fatalf("%q %v", d, err)
	}
	for _, p := range []string{root, d} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
	}
	for _, bad := range []string{"../escape", "sbx_0123456789abcdef/..", ""} {
		if _, err := get(bad); err == nil {
			t.Errorf("id %q was given a folder", bad)
		}
		if err := release(bad); err == nil {
			t.Errorf("id %q was released", bad)
		}
	}
	if err := release("sbx_0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d); !os.IsNotExist(err) {
		t.Fatalf("not removed: %v", err)
	}
	// A root others may write is refused when a sandbox needs it.
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := get("sbx_0123456789abcdef"); err == nil || !strings.Contains(err.Error(), "written by others") {
		t.Fatalf("a world-writable root: %v", err)
	}
}

// Unset, each sandbox gets a folder of its own in the control plane's state
// folder, never the server's workspace or ~/.abhed.
func TestSandboxWorkspacesDefaultToTheirOwn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	ws := t.TempDir()
	get, release, err := sandboxWorkspaces(config.ControlPlaneConfig{}, ws)
	if err != nil || release == nil {
		t.Fatal(err)
	}
	d, err := get("sbx_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(state, "abhed", "control-plane", "sandboxes", "sbx_0123456789abcdef")
	if runtime.GOOS == "darwin" {
		want = filepath.Join(home, "Library", "Application Support", "Abhed", "control-plane", "sandboxes", "sbx_0123456789abcdef")
	}
	if d != want || within(d, ws) || within(d, filepath.Join(home, ".abhed")) {
		t.Fatalf("default folder %q, want %q", d, want)
	}
	other, _ := get("sbx_fedcba9876543210")
	if other == d {
		t.Fatal("two sandboxes share a folder")
	}
}

// The control plane's drivers are the ones the configuration describes: a
// configured microvm driver is selectable by name, and one that cannot be
// reached refuses its sandboxes rather than being missing.
func TestControlPlaneDriversFromConfig(t *testing.T) {
	cfg := config.Default()
	r, err := controlPlaneDrivers(context.Background(), nil, cfg, builtin.Options{})
	if err != nil || !slices.Equal(r.Names(), []string{"process"}) {
		t.Fatalf("%v %v", r.Names(), err)
	}
	cfg.Sandbox.MicroVMSocket = filepath.Join(t.TempDir(), "absent.sock")
	var warn bytes.Buffer
	r, err = controlPlaneDrivers(context.Background(), &warn, cfg, builtin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, err := r.Get("microvm")
	if err != nil {
		t.Fatalf("the configured microvm driver is not registered: %v", err)
	}
	if _, err := d.Create(context.Background(), driver.Spec{ID: "sbx_0123456789abcdef"}); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("an unreachable driver did not fail closed: %v", err)
	}
	if !strings.Contains(warn.String(), "sandbox driver microvm is unavailable") {
		t.Fatalf("no warning: %q", warn.String())
	}
}

// serve's control plane snapshots through the process driver into its
// state folder, records the driver's own event, and exposes only when on.
func TestBuiltControlPlaneSnapshotsAndRecords(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := config.Default()
	cfg.Sandbox.MinTier = "none"
	ws := t.TempDir()
	built, err := buildControlPlane(context.Background(), cfg, ws, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if built.expose != nil {
		t.Fatal("exposures are on by default")
	}
	ctx := context.Background()
	who := controlplane.Caller{Principal: "alice"}
	sb, err := built.svc.Create(ctx, who, controlplane.CreateRequest{Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if err := built.svc.Reconcile(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	ref, _, err := built.svc.Snapshot(ctx, who, sb.ID, "", "first")
	if err != nil {
		t.Fatalf("snapshot through serve's process driver: %v", err)
	}
	dir, _ := snapshotDir(cfg.ControlPlane)
	stateDir, _ := controlPlaneStateDir()
	if !within(dir, stateDir) || within(dir, ws) {
		t.Fatalf("snapshots in %s", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, sb.ID)); err != nil {
		t.Fatalf("no snapshot folder: %v", err)
	}
	evs, _ := built.svc.Store().(controlplane.AuditReader).AuditFor(ctx, sb.ID)
	recorded := false
	for _, ev := range evs {
		recorded = recorded || ev.Action == driver.EventSnapshot && ev.Principal == controlplane.DriverPrincipal && ev.Detail["digest"] == ref.Digest
	}
	if !recorded {
		t.Fatalf("the driver's snapshot is not in the record: %+v", evs)
	}

	cfg.ControlPlane.Expose = true
	if built, err = buildControlPlane(context.Background(), cfg, ws, nil, nil); err != nil || built.expose == nil || !built.expose.Enabled() {
		t.Fatalf("exposures asked for: %+v %v", built.expose, err)
	}
	cfg.ControlPlane.SnapshotDir = "relative"
	if _, err := buildControlPlane(context.Background(), cfg, ws, nil, nil); err == nil {
		t.Fatal("a relative snapshot folder was accepted")
	}
}
