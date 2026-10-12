package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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

// ServerCommand confines argv's files (see serverFiles) and, under the
// egress allowlist, its network: it reaches nothing but its proxy, which
// lives as long as ctx. With the allowlist off its network is as it was.
func (s *Process) ServerCommand(ctx context.Context, name string, argv, env []string, record func(string, map[string]any) error) (*exec.Cmd, error) {
	return s.serverCommand(ctx, name, argv, env, record, true)
}

// ServerNetCommand is ServerCommand with the server's files as the host's,
// for a server configured with host_files; its network is still confined
// under the allowlist.
func (s *Process) ServerNetCommand(ctx context.Context, name string, argv, env []string, record func(string, map[string]any) error) (*exec.Cmd, error) {
	return s.serverCommand(ctx, name, argv, env, record, false)
}

func (s *Process) serverCommand(ctx context.Context, name string, argv, env []string, record func(string, map[string]any) error, confineFiles bool) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("sandbox: no command")
	}
	var files serverFiles
	if confineFiles {
		var err error
		if files, err = s.serverFilesFor(name, argv[0], env); err != nil {
			return nil, err
		}
		if s.policy.Egress == nil {
			return s.serverFilesOnly(ctx, files, argv, env)
		}
	} else if s.policy.Egress == nil {
		return nil, errors.New("sandbox: no egress policy to confine a server to")
	}
	if why := s.egressAvailable(); why != "" {
		return nil, errors.New(why)
	}
	if s.backend == "bwrap" {
		if err := serverRootRefusal(os.Getuid(), os.Geteuid()); err != nil {
			return nil, err
		}
	}
	key := ServerKey(name)
	// The server's own credential, revoked when ctx ends.
	e, call, err := s.egressFor(WithLaunch(ctx, Launch{CallID: key, Session: key, Record: record}))
	if err != nil {
		return nil, fmt.Errorf("the egress proxy is not available: %w", err)
	}
	env = append(serverEnv(env), call.Env()...)
	if confineFiles {
		env = serverHomeEnv(env, files.home)
	}
	var cmd *exec.Cmd
	switch s.backend {
	case "sandbox-exec":
		profile := serverProfile(e.portFor(call)) + files.seatbeltRules()
		cmd = exec.CommandContext(ctx, "sandbox-exec", append([]string{"-p", profile}, argv...)...) // #nosec G204 -- the configured server, confined
	case "bwrap":
		// A host /proc would show every process's environment and sockets.
		if !s.bwrapFreshOK() {
			return nil, errors.New("sandbox: bubblewrap cannot mount a private /proc here, so a server's processes cannot be hidden from it")
		}
		args := append([]string{"--die-with-parent", "--unshare-net", "--unshare-pid", "--dev-bind", "/", "/", "--proc", "/proc"},
			serverHides(e.dir, e.exe)...)
		if confineFiles {
			args = append(args, files.bwrapArgs()...)
		}
		sock := filepath.Join(e.dir, "proxy.sock")
		if call.Socket() != "" {
			sock = call.Socket() // the server's own, as a command's
		}
		relay := []string{e.exe, egress.RelayArg, sock, e.proxy.Addr().String()}
		if e.dns {
			// The session's resolver, as commands get it; its socket is in the folder bound back.
			args = append(args, "--ro-bind", filepath.Join(e.dir, "resolv.conf"), "/etc/resolv.conf")
			args = append(args, relayCaps()...)
			relay = append(relay, egress.DNSArg, filepath.Join(e.dir, "dns.sock"), strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid()))
		}
		args = append(append(args, relay...), "--")
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
	b.WriteString(denyLaunchServices)
	return b.String()
}

// denySessionServices keeps a command from the person's clipboard
// (pbpaste, pbcopy) and from capturing their screen through the window server.
const denySessionServices = `(deny mach-lookup (global-name-prefix "com.apple.pasteboard.") (global-name "com.apple.windowserver.active") (global-name-prefix "com.apple.screencapture"))` + "\n"

// denyLaunchServices keeps LaunchServices and Apple events from a profile, which would
// open a URL or run an app outside it; the coreservices/lsd part in any case, as Seatbelt names are case-sensitive.
const denyLaunchServices = `(deny mach-lookup (global-name-regex #"^com\.apple\.[cC][oO][rR][eE][sS][eE][rR][vV][iI][cC][eE][sS]\.") (global-name-regex #"^com\.apple\.[lL][sS][dD]\."))` + "\n"

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

// serverRunDirs hold the session bus, user services, container engines' and
// resolvers' sockets; each is hidden under an empty tmpfs.
var serverRunDirs = []string{"/run/user", "/run/dbus", "/run/podman", "/run/docker", "/run/containerd",
	"/run/systemd/resolve", "/run/nscd", "/run/cups", "/run/avahi-daemon"}

// serverRunSockets are container engines' sockets, each hidden by /dev/null.
var serverRunSockets = []string{"/run/docker.sock", "/run/podman.sock", "/run/containerd.sock", "/run/snapd.socket", "/run/snapd-snap.socket"}

// serverHides hides the run folders, XDG_RUNTIME_DIR and the temp folder
// (other sessions' egress sockets), binding the server's socket and relay back.
func serverHides(sockDir, exe string) []string {
	return append(serverRunHides(), "--bind", sockDir, sockDir, "--ro-bind", exe, exe)
}

// serverRunHides hides the run folders, XDG_RUNTIME_DIR, the container
// engines' sockets and the temp folder.
func serverRunHides() []string {
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
		if info, err := os.Stat(d); err == nil && info.IsDir() { // #nosec G703 -- a stat of a run or temp folder, only to hide it
			args = append(args, "--tmpfs", d)
		}
	}
	for _, f := range serverRunSockets {
		if _, err := os.Stat(f); err == nil {
			args = append(args, "--ro-bind", "/dev/null", f)
		}
	}
	return args
}

// serverRootRefusal refuses a stdio server under bubblewrap when Abhed runs
// as root: the server would keep root's capabilities over the host's files.
func serverRootRefusal(uid, euid int) error {
	if uid == 0 || euid == 0 {
		return errors.New("sandbox: Abhed runs as root, and a stdio MCP server under bubblewrap would keep root's " +
			"capabilities and a writable host filesystem, so it is not started; run Abhed as an ordinary user")
	}
	return nil
}
