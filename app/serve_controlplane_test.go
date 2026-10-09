package app

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
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
	db := filepath.Join(os.Getenv("HOME"), ".abhed", "controlplane", "sandboxes.db")
	if info, err := os.Stat(db); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the database: %v %v", info, err)
	}
	for _, want := range []string{
		"control plane", "on, store sqlite (",
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
	get, release, err := sandboxWorkspaces("", ws)
	if err != nil || release != nil {
		t.Fatal(err)
	}
	if d, _ := get("sbx_0123456789abcdef"); d != ws {
		t.Fatalf("shared workspace %q", d)
	}
	if _, _, err := sandboxWorkspaces("relative/root", ws); err == nil {
		t.Fatal("a relative root was accepted")
	}
	root := t.TempDir()
	get, release, err = sandboxWorkspaces(root, ws)
	if err != nil {
		t.Fatal(err)
	}
	d, err := get("sbx_0123456789abcdef")
	if err != nil || d != filepath.Join(root, "sbx_0123456789abcdef") {
		t.Fatalf("%q %v", d, err)
	}
	if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("workspace %v %v", fi, err)
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
	svc, label, closeCP, err := buildControlPlane(ctx, cfg, ws, agent.NewMemStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(label, "sqlite (") {
		t.Fatalf("store %q", label)
	}
	sb, err := controlplane.NewSandbox("alice", "default", "process", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store().Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	closeCP()
	svc, _, closeCP, err = buildControlPlane(ctx, cfg, ws, agent.NewMemStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeCP()
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
			if _, _, _, err := buildControlPlane(context.Background(), cfg, ws, agent.NewMemStore(), nil); err == nil {
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
