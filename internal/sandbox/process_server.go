package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
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
	env = append(append([]string{}, env...), e.proxy.Env(key)...)
	var cmd *exec.Cmd
	switch s.backend {
	case "sandbox-exec":
		cmd = exec.CommandContext(ctx, "sandbox-exec", append([]string{"-p", serverProfile(e.port())}, argv...)...) // #nosec G204 -- the configured server, confined
	case "bwrap":
		// The whole filesystem as it is, so the socket's folder and this
		// binary, run as the relay, are where they are outside.
		args := []string{"--die-with-parent", "--unshare-net", "--dev-bind", "/", "/",
			e.exe, egress.RelayArg, filepath.Join(e.dir, "proxy.sock"), e.proxy.Addr().String(), "--"}
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
	return b.String()
}
