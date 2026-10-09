package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// SSHHostKeyName is the :ssh relay's own host key in the control plane's
// state folder.
const SSHHostKeyName = "ssh_host_ed25519_key"

// ControlPlaneStateDir is where serve keeps the control plane's own folders:
// outside ~/.abhed and any workspace, in Application Support on macOS and
// in $XDG_STATE_HOME (~/.local/state without it) elsewhere.
func ControlPlaneStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("the control plane's state folder: %w", err)
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Abhed", "control-plane"), nil
	}
	if x := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "abhed", "control-plane"), nil
	}
	return filepath.Join(home, ".local", "state", "abhed", "control-plane"), nil
}

// SSHHostKeyFile is the :ssh relay's host key: control_plane.ssh_host_key,
// which must be absolute, or else the server's own in the state folder.
func (c Config) SSHHostKeyFile() (string, error) {
	if p := c.ControlPlane.SSHHostKey; p != "" {
		if !filepath.IsAbs(p) {
			return p, fmt.Errorf("control_plane.ssh_host_key %q is not an absolute path", p)
		}
		return filepath.Clean(p), nil
	}
	state, err := ControlPlaneStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, SSHHostKeyName), nil
}

// SnapshotDir is where sandbox snapshots are kept: control_plane.snapshot_dir,
// which must be absolute, or else snapshots in the state folder.
func (c Config) SnapshotDir() (string, error) {
	if p := c.ControlPlane.SnapshotDir; p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("control_plane.snapshot_dir %q is not an absolute path", p)
		}
		return filepath.Clean(p), nil
	}
	state, err := ControlPlaneStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "snapshots"), nil
}

// validateControlPlanePaths refuses a relative host key or snapshot folder
// when the configuration loads, not when serve first needs it.
func (c Config) validateControlPlanePaths() error {
	for _, k := range [][2]string{{"ssh_host_key", c.ControlPlane.SSHHostKey}, {"snapshot_dir", c.ControlPlane.SnapshotDir}} {
		if k[1] != "" && !filepath.IsAbs(k[1]) {
			return fmt.Errorf("control_plane.%s %q is not an absolute path", k[0], k[1])
		}
	}
	return nil
}
