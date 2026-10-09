package app

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
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
