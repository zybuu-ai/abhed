package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// ServerLauncher builds an MCP stdio server's command, its network confined
// to an egress proxy of its own whose decisions go to record.
type ServerLauncher interface {
	ServerCommand(ctx context.Context, name string, argv, env []string, record func(string, map[string]any) error) (*exec.Cmd, error)
}

// ServerKey keys server name's proxy apart from the sessions' and is its call
// id; no colon, which would end the user name in Basic credentials.
func ServerKey(name string) string { return "mcp/" + name }

// ServerCommand confines only argv's network: it reaches nothing but its
// proxy, which lives as long as ctx. Its files are not confined.
func (s *Process) ServerCommand(ctx context.Context, name string, argv, env []string, record func(string, map[string]any) error) (*exec.Cmd, error) {
	if s.policy.Egress == nil {
		return nil, errors.New("sandbox: no egress policy to confine a server to")
	}
	if why := s.egressAvailable(); why != "" {
		return nil, errors.New(why)
	}
	if len(argv) == 0 {
		return nil, errors.New("sandbox: no command")
	}
	key := ServerKey(name)
	e, err := s.egressFor(WithLaunch(ctx, Launch{CallID: key, Session: key, Record: record}))
	if err != nil {
		return nil, fmt.Errorf("the egress proxy is not available: %w", err)
	}
	env = append(serverEnv(env), e.proxy.Env(key)...)
	var cmd *exec.Cmd
	switch s.backend {
	case "sandbox-exec":
		cmd = exec.CommandContext(ctx, "sandbox-exec", append([]string{"-p", serverProfile(e.port())}, argv...)...) // #nosec G204 -- the configured server, confined
	case "bwrap":
		// A host /proc would show every process's environment and sockets.
		if !s.bwrapFreshOK() {
			return nil, errors.New("sandbox: bubblewrap cannot mount a private /proc here, so a server's processes cannot be hidden from it")
		}
		args := append([]string{"--die-with-parent", "--unshare-net", "--unshare-pid", "--dev-bind", "/", "/", "--proc", "/proc"},
			serverHides(e.dir, e.exe)...)
		args = append(args, e.exe, egress.RelayArg, filepath.Join(e.dir, "proxy.sock"), e.proxy.Addr().String(), "--")
		cmd = exec.CommandContext(ctx, "bwrap", append(args, argv...)...) // #nosec G204 -- the configured server, confined
	default:
		return nil, fmt.Errorf("sandbox: the %s backend cannot confine a server's network", s.backend)
	}
	cmd.Env = env
	return cmd, nil
}

// serverProfile is a Seatbelt profile that denies every network use but
// outbound to the proxy's port on localhost, and nothing else.
func serverProfile(port uint16) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n(deny network*)\n")
	fmt.Fprintf(&b, "(allow network-outbound (remote ip \"localhost:%d\"))\n", port)
	b.WriteString("(deny sysctl-read (sysctl-name-prefix \"net.route\"))\n")
	b.WriteString("(deny system-socket (socket-domain AF_ROUTE))\n")
	b.WriteString("(deny mach-lookup (global-name-prefix \"com.apple.SystemConfiguration\") (global-name-prefix \"com.apple.network\"))\n")
	// LaunchServices would open a URL or an app outside the sandbox for it.
	b.WriteString("(deny mach-lookup (global-name \"com.apple.coreservices.launchservicesd\") (global-name-prefix \"com.apple.lsd.\"))\n")
	return b.String()
}

// serverEnvDropped are variables naming a session bus, an agent or a
// container engine: ways out a server is not handed.
var serverEnvDropped = []string{"DBUS_SESSION_BUS_ADDRESS", "DBUS_SYSTEM_BUS_ADDRESS", "XDG_RUNTIME_DIR",
	"SSH_AUTH_SOCK", "GPG_AGENT_INFO", "DOCKER_HOST", "CONTAINER_HOST", "PODMAN_HOST", "WAYLAND_DISPLAY", "DISPLAY"}

// serverEnv is env without serverEnvDropped and the proxy variables, which
// the server's own proxy sets.
func serverEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(serverEnvDropped, k) || strings.EqualFold(k, "http_proxy") || strings.EqualFold(k, "https_proxy") ||
			strings.EqualFold(k, "no_proxy") || strings.EqualFold(k, "all_proxy") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// serverRunDirs hold the session bus, user services and container engines'
// sockets; each is hidden under an empty tmpfs.
var serverRunDirs = []string{"/run/user", "/run/dbus", "/run/podman", "/run/docker", "/run/containerd"}

// serverRunSockets are container engines' sockets, each hidden by /dev/null.
var serverRunSockets = []string{"/run/docker.sock", "/run/podman.sock", "/run/containerd.sock"}

// serverHides hides the run folders, XDG_RUNTIME_DIR and the temp folder
// (other sessions' egress sockets), binding the server's socket and relay back.
func serverHides(sockDir, exe string) []string {
	var args []string
	dirs := append([]string{}, serverRunDirs...)
	if x := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(x) {
		dirs = append(dirs, x)
	}
	tmps := []string{"/tmp"}
	if t := os.TempDir(); filepath.IsAbs(t) {
		tmps = append(tmps, t)
	}
	seen := map[string]bool{}
	for _, d := range append(dirs, tmps...) {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			d = r
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			args = append(args, "--tmpfs", d)
		}
	}
	for _, f := range serverRunSockets {
		if _, err := os.Stat(f); err == nil {
			args = append(args, "--ro-bind", "/dev/null", f)
		}
	}
	return append(args, "--bind", sockDir, sockDir, "--ro-bind", exe, exe)
}
