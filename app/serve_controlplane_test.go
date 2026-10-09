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
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/builtin"
	"github.com/zybuu-ai/abhed/internal/agent"
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
	// The check creates nothing: the folder and file are only named.
	db := filepath.Join(os.Getenv("HOME"), ".abhed", "controlplane", "sandboxes.db")
	if _, err := os.Lstat(filepath.Dir(db)); !os.IsNotExist(err) {
		t.Fatalf("serve -check created the database's folder: %v", err)
	}
	for _, want := range []string{
		"control plane", "on, store sqlite (would be created at " + db + ")",
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

// An edition's control-plane hook sees the configuration before the service
// is built, and its error refuses the start.
func TestControlPlaneHooks(t *testing.T) {
	ws := checkEnv(t, `{"control_plane":{"enabled":true}}`, "")
	var saw []string
	hook := WithControlPlane(func(_ context.Context, _ config.Config, c *controlplane.Config) error {
		saw = append(saw, c.DefaultDriver)
		c.NodeID = "node-a"
		return nil
	})
	var out bytes.Buffer
	if code := newApp(hook).serveCheck(&out, ws, "127.0.0.1:0"); code != 0 || len(saw) != 1 || saw[0] != "process" {
		t.Fatalf("exit %d, hook saw %v:\n%s", code, saw, out.String())
	}
	refuse := WithControlPlane(func(context.Context, config.Config, *controlplane.Config) error {
		return errors.New("the relay key is missing")
	})
	out.Reset()
	if code := newApp(refuse).serveCheck(&out, ws, "127.0.0.1:0"); code != 1 || !strings.Contains(out.String(), "the relay key is missing") {
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
	cfg.ControlPlane.Enabled = true
	cfg.Sandbox.MinTier = "none"
	ws := t.TempDir()
	built, err := buildControlPlane(context.Background(), cfg, ws, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer built.close()
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
	dir, _ := snapshotDir(cfg)
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
	again, err := buildControlPlane(context.Background(), cfg, ws, nil, nil)
	if err != nil || again.expose == nil || !again.expose.Enabled() {
		t.Fatalf("exposures asked for: %+v %v", again.expose, err)
	}
	again.close()
	cfg.ControlPlane.SnapshotDir = "relative"
	if _, err := buildControlPlane(context.Background(), cfg, ws, nil, nil); err == nil {
		t.Fatal("a relative snapshot folder was accepted")
	}
}

// Without Postgres the control plane keeps sandboxes in SQLite, so they
// outlive the server; it never falls back to memory.
func TestControlPlaneSQLiteSurvivesRestart(t *testing.T) {
	ws := checkEnv(t, `{"control_plane":{"enabled":true}}`, "")
	cfg, err := newApp().loadConfig(ws)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	built, err := buildControlPlane(ctx, cfg, ws, agent.NewMemStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	svc, closeCP := built.svc, built.close
	if !strings.HasPrefix(built.label, "sqlite (") {
		t.Fatalf("store %q", built.label)
	}
	sb, err := controlplane.NewSandbox("alice", "default", "process", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store().Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	closeCP()
	built, err = buildControlPlane(ctx, cfg, ws, agent.NewMemStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	svc = built.svc
	defer built.close()
	if _, err := svc.Store().Get(ctx, sb.ID); err != nil {
		t.Fatalf("after a restart: %v", err)
	}
}

// The database is refused where the agent's or a sandbox's commands write.
func TestControlPlaneSQLitePathRefused(t *testing.T) {
	root := t.TempDir()
	for name, path := range map[string]string{
		"relative":        "sandboxes.db",
		"workspace":       "WS/state/sandboxes.db",
		"workspace abhed": "WS/.abhed/sandboxes.db",
		"sandbox folder":  root + "/cp/sandboxes.db",
	} {
		t.Run(name, func(t *testing.T) {
			ws := checkEnv(t, "{}", "")
			path = strings.ReplaceAll(path, "WS", ws)
			cfgText := `{"control_plane":{"enabled":true,"workspace_root":"` + root + `","sqlite_path":"` + path + `"}}`
			if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".abhed", "config.json"), []byte(cfgText), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := newApp().loadConfig(ws)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := buildControlPlane(context.Background(), cfg, ws, agent.NewMemStore(), nil); err == nil {
				t.Fatalf("%s was accepted", path)
			}
			var out bytes.Buffer
			if code := newApp().serveCheck(&out, ws, "127.0.0.1:0"); code != 1 || !strings.Contains(out.String(), "FAIL control plane") {
				t.Fatalf("exit %d:\n%s", code, out.String())
			}
			if filepath.IsAbs(path) {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("the refused database was created: %v", err)
				}
			}
		})
	}
}

func TestControlPlaneLabel(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for want, cfg := range map[string]config.Config{
		"off (control_plane.enabled)": {},
		"postgres (tenant acme, beside the sessions)": {ControlPlane: config.ControlPlaneConfig{Enabled: true},
			Storage: config.StorageConfig{Driver: "postgres", Tenant: "acme"}},
		"sqlite (/home/u/.abhed/controlplane/sandboxes.db)": {ControlPlane: config.ControlPlaneConfig{Enabled: true}},
		"sqlite (/srv/abhed/cp.db)":                         {ControlPlane: config.ControlPlaneConfig{Enabled: true, SQLitePath: "/srv/abhed/cp.db"}},
	} {
		if got := controlPlaneLabel(cfg); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// serve -check reads an existing database and says whether a migration is
// due, applying none; once it is current it says so.
func TestServeCheckDoesNotMigrate(t *testing.T) {
	ws := checkEnv(t, `{"control_plane":{"enabled":true}}`, "")
	db := filepath.Join(os.Getenv("HOME"), ".abhed", "controlplane", "sandboxes.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := newApp().serveCheck(&out, ws, "127.0.0.1:0"); code != 0 || !strings.Contains(out.String(), "schema version 0; serve will migrate it to") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if info, err := os.Stat(db); err != nil || info.Size() != 0 {
		t.Fatalf("serve -check wrote to the database: %v %v", info, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(db)); len(entries) != 1 {
		t.Fatalf("serve -check left files beside the database: %v", entries)
	}

	cfg, err := newApp().loadConfig(ws)
	if err != nil {
		t.Fatal(err)
	}
	built, err := buildControlPlane(context.Background(), cfg, ws, agent.NewMemStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	built.close()
	real, err := filepath.EvalSymlinks(db)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := newApp().serveCheck(&out, ws, "127.0.0.1:0"); code != 0 || !strings.Contains(out.String(), "on, store sqlite ("+real+", schema version ") ||
		strings.Contains(out.String(), "migrate") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
}

// serve hands the process driver its own workspace, so a sandbox's commands
// cannot read serve's accounts in that workspace's .abhed.
func TestServeSandboxCannotReadServeState(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the host's sandbox")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	users := filepath.Join(ws, ".abhed", "users.json")
	if err := os.MkdirAll(filepath.Dir(users), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(users, []byte(`{"alice":"hash"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ControlPlane.Enabled = true
	cfg.Sandbox.MinTier = "process"
	built, err := buildControlPlane(context.Background(), cfg, ws, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer built.close()
	ctx := context.Background()
	who := controlplane.Caller{Principal: "alice"}
	sb, err := built.svc.Create(ctx, who, controlplane.CreateRequest{Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if err := built.svc.Reconcile(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	res, _, err := built.svc.Exec(ctx, who, sb.ID, "", controlplane.ExecRequest{Command: "cat " + users},
		controlplane.ExecIO{Stdout: &out, Stderr: &out})
	if errors.Is(err, driver.ErrUnavailable) || errors.Is(err, controlplane.ErrUnavailable) {
		if _, statErr := os.Stat("/usr/bin/sandbox-exec"); statErr == nil {
			t.Fatalf("Seatbelt is present but the sandbox did not run: %v", err)
		}
		t.Skipf("no process tier on this host: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 || strings.Contains(out.String(), "alice") {
		t.Fatalf("a sandbox read serve's accounts: %d %q", res.ExitCode, out.String())
	}
}
