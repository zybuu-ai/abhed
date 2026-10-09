package sandboxconfig

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// controlPlaneHome points the control plane's state folder at a fresh home
// and returns that folder.
func controlPlaneHome(t *testing.T, base string) string {
	t.Helper()
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "home", ".local", "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "home", ".config"))
	t.Setenv("ABHED_SECRETS_FILE", "")
	state, err := config.ControlPlaneStateDir()
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// A session's policy denies the control plane's state folder and a
// snapshot_dir kept elsewhere; a sandbox whose folder lies in the state
// folder has the rest of it hidden around its own.
func TestPolicyDeniesControlPlaneState(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := controlPlaneHome(t, base)
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ControlPlane.Enabled = true
	cfg.ControlPlane.SnapshotDir = filepath.Join(base, "snaps")
	p, err := Policy(cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{state, cfg.ControlPlane.SnapshotDir} {
		if !slices.Contains(p.StatePaths, want) {
			t.Errorf("a session's policy leaves %s open: %v", want, p.StatePaths)
		}
	}
	own := filepath.Join(state, "sandboxes", "sbx_a")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err = Policy(cfg, own)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(p.Hidden, state) || slices.Contains(p.StatePaths, state) {
		t.Errorf("a sandbox in the state folder: hidden %v, state %v", p.Hidden, p.StatePaths)
	}
	if !slices.Contains(p.StatePaths, cfg.ControlPlane.SnapshotDir) {
		t.Errorf("a sandbox's policy leaves the snapshots open: %v", p.StatePaths)
	}
	// The file tools refuse the host key and the snapshots, not the folder.
	tp := ToolStatePaths(cfg, ws)
	for _, want := range []string{filepath.Join(state, config.SSHHostKeyName), cfg.ControlPlane.SnapshotDir} {
		if !slices.Contains(tp, want) {
			t.Errorf("the file tools may reach %s: %v", want, tp)
		}
	}
	if slices.Contains(tp, state) {
		t.Error("the whole state folder is refused to the file tools, per-session workspaces included")
	}
}

// On the host's own sandbox a session command can neither read nor write
// the relay's host key, default or configured, nor any snapshot.
func TestSessionCannotReachControlPlaneState(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the host's sandbox")
	}
	// Outside temp areas: a configured key there is refused at start.
	base := outsideWritableAreas(t)
	state := controlPlaneHome(t, base)
	ws := filepath.Join(base, "ws")
	snaps := filepath.Join(base, "snaps")
	keys := filepath.Join(base, "keys")
	secret := map[string]string{
		filepath.Join(state, config.SSHHostKeyName):            "default-host-key",
		filepath.Join(state, "snapshots", "sbx_b", "snap.tar"): "sibling-snapshot",
		filepath.Join(snaps, "sbx_b", "snap.tar"):              "configured-snapshot",
		filepath.Join(keys, "relay_key"):                       "configured-host-key",
	}
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	for p, body := range secret {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.Sandbox.MinTier = "process"
	cfg.ControlPlane.Enabled = true
	cfg.ControlPlane.SnapshotDir = snaps
	cfg.ControlPlane.SSHHostKey = filepath.Join(keys, "relay_key")
	sb, err := Build(cfg, ws)
	if err != nil {
		if _, statErr := os.Stat("/usr/bin/sandbox-exec"); statErr == nil {
			t.Fatalf("Seatbelt is present but no sandbox was built: %v", err)
		}
		t.Skipf("no process sandbox here: %v", err)
	}
	defer func() { _ = sandbox.Close(sb) }()
	if out, err := sb.Command(context.Background(), ws, "echo ok > mine && cat mine").CombinedOutput(); err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("the sandbox cannot run a command in its workspace: %v %s", err, out)
	}
	for p, body := range secret {
		out, _ := sb.Command(context.Background(), ws, "cat '"+p+"'; ls '"+filepath.Dir(p)+"' 2>/dev/null | sed 's/^/LS:/'; echo x > '"+p+"'").CombinedOutput()
		// The configured key's folder is not state; the key alone is.
		listed := filepath.Dir(p) != keys && strings.Contains(string(out), "LS:"+filepath.Base(p))
		if strings.Contains(string(out), body) || listed {
			t.Errorf("a session command reached %s: %s", p, out)
		}
		if got, _ := os.ReadFile(p); string(got) != body {
			t.Errorf("a session command rewrote %s: %q", p, got)
		}
	}
}
